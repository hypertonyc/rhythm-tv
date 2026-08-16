package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/httpapi/web"
	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/library"
	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/mediasource"
	"github.com/avdav/torrent-media/server/internal/subs"
)

// Маршруты. Классы символов расписаны по буквам там, где в оригинале стоял
// флаг /i: в Go (?i) делает юникодный fold, и (?i)[a-z] совпал бы, например,
// с KELVIN SIGN, а JS-овый /i без /u — нет.
var (
	reProbe     = regexp.MustCompile(`^/api/probe/(\d+)$`)
	rePrebuffer = regexp.MustCompile(`^/api/prebuffer/(\d+)$`)
	reRaw       = regexp.MustCompile(`^/raw/(\d+)$`)
	reStart     = regexp.MustCompile(`^/api/start/(\d+)$`)
	reHLSStatus = regexp.MustCompile(`^/api/hls-status/([A-Za-z0-9-]+)$`)
	reHLSFile   = regexp.MustCompile(
		`^/hls/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+\.(?:[Mm]3[Uu]8|[Tt][Ss]|[Vv][Tt][Tt]))$`)
	// Библиотека торрентов. Этих маршрутов в Node-эталоне нет вовсе, поэтому
	// они не участвуют в сверке контракта — и телевизор в них не ходит.
	reTorrentAction = regexp.MustCompile(`^/api/torrents/([0-9a-f]{40})/(activate|delete)$`)
)

const prebufferBytes = 8 << 20

// Torrents — библиотека торрентов глазами HTTP-слоя.
//
// Интерфейс, а не *library.Library, ровно по той же причине, по которой
// торрент прячется за mediasource.Source: обработчики должны проверяться
// без каталога с настоящими .torrent на диске.
type Torrents interface {
	// Current отдаёт активный источник или nil, если торрент не выбран.
	// nil — законное состояние: снаружи оно неотличимо от «метаданные грузятся».
	Current() mediasource.Source
	ActiveID() string
	List() ([]library.Entry, error)
	Add(r io.Reader) (library.Entry, error)
	Activate(id string) (library.Entry, error)
	Remove(id string, withData bool) error
}

// Sessions — сеансы перекодирования глазами HTTP-слоя; это *hls.Manager.
//
// Интерфейс нужен ради одной проверки, которую иначе не написать: смена
// активного торрента ОБЯЗАНА гасить ffmpeg до переключения источника,
// а на настоящем менеджере такой тест потребовал бы живого ffmpeg.
type Sessions interface {
	Start(opts hls.StartOptions) (hls.Snapshot, error)
	Stop() *string
	Get(id string) (hls.Snapshot, bool)
	ActiveSnapshot() *hls.Snapshot
	SessionDir(id string) (string, bool)
}

// Deps — всё, что нужно обработчикам.
type Deps struct {
	// Library владеет активным источником; обработчики спрашивают его один раз
	// в начале запроса и дальше работают с локальной переменной. Так индекс
	// файла не может «переехать» на другой торрент посреди обработки.
	Library Torrents
	Prober  *media.Prober
	HLS     Sessions
	// Subs — субтитры, лежащие отдельными файлами рядом с сервером. nil
	// означает «каталога нет», и тогда всё работает как раньше.
	Subs *subs.Library
	// BaseCtx живёт столько же, сколько процесс. Нужен префетчу: контекст
	// запроса там не годится (см. handlePrebuffer).
	BaseCtx context.Context
}

// Server — обработчик всех маршрутов.
type Server struct {
	deps Deps

	prebufMu sync.Mutex
	prebuf   map[string]struct{}
}

func New(deps Deps) *Server {
	return &Server{deps: deps, prebuf: make(map[string]struct{})}
}

// source отдаёт активный источник запроса.
//
// Второе значение — «есть готовый торрент». Пустая библиотека и торрент
// без метаданных сведены здесь в одно состояние сознательно: контракт
// с телевизором знает ровно два ответа (503 на /api/files, ready:false
// на /api/status), и заводить третий значило бы менять его.
func (s *Server) source() (mediasource.Source, bool) {
	if s.deps.Library == nil {
		return nil, false
	}
	src := s.deps.Library.Current()
	if src == nil || !src.Ready() {
		return src, false
	}
	return src, true
}

// ServeHTTP — линейная цепочка проверок в том же порядке, что и в оригинале.
//
// http.ServeMux здесь использовать НЕЛЬЗЯ: он чистит путь и отвечает 301
// на «//api/files» и «/api/./files», тогда как оригинал просто сравнивал
// строки и отдавал 404. Метод тоже не проверяется нигде, кроме OPTIONS:
// POST /api/stop работает сегодня, и ломать это незачем.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, а не Path: Node сравнивал с процентно-закодированным
	// pathname, поэтому «/api/probe/%31» у него не совпадал с шаблоном
	// и давал 404, а Path декодировал бы его в «/api/probe/1» и дал 200.
	path := r.URL.EscapedPath()

	if r.Method == http.MethodOptions {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET,HEAD,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch path {
	case "/":
		writeText(w, http.StatusOK, string(web.IndexHTML), contentTypeHTML)
		return
	case "/api/files":
		s.handleFiles(w)
		return
	case "/api/status":
		s.handleStatus(w)
		return
	case "/api/stop":
		s.handleStop(w)
		return
	case "/api/torrents":
		s.handleTorrents(w, r)
		return
	}

	if m := reTorrentAction.FindStringSubmatch(path); m != nil {
		s.handleTorrentAction(w, r, m[1], m[2])
		return
	}

	if m := reProbe.FindStringSubmatch(path); m != nil {
		s.handleProbe(w, r, m[1])
		return
	}
	if m := rePrebuffer.FindStringSubmatch(path); m != nil {
		s.handlePrebuffer(w, m[1])
		return
	}
	if m := reRaw.FindStringSubmatch(path); m != nil {
		// Ready здесь не проверяется, и это поведение оригинала: без метаданных
		// список файлов пуст, индекс не проходит границы и получается 404.
		src := s.currentSource()
		index, ok := s.parseIndex(src, m[1])
		if !ok {
			writeText(w, http.StatusNotFound, "File not found", contentTypeText)
			return
		}
		s.serveRaw(w, r, src, index)
		return
	}
	if m := reStart.FindStringSubmatch(path); m != nil {
		s.handleStart(w, r, m[1])
		return
	}
	if m := reHLSStatus.FindStringSubmatch(path); m != nil {
		snap, ok := s.deps.HLS.Get(m[1])
		if !ok {
			errorJSON(w, http.StatusNotFound, "HLS session not found")
			return
		}
		writeJSON(w, http.StatusOK, snap)
		return
	}
	if m := reHLSFile.FindStringSubmatch(path); m != nil {
		s.serveHLSFile(w, r, m[1], m[2])
		return
	}

	writeText(w, http.StatusNotFound, "Not found", contentTypeText)
}

type fileEntry struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

func (s *Server) handleFiles(w http.ResponseWriter) {
	src, ok := s.source()
	if !ok {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	list := s.videoFiles(src)
	files := make([]fileEntry, 0, len(list))
	for _, f := range list {
		files = append(files, fileEntry{Index: f.Index, Name: f.Name, Length: f.Length})
	}
	writeJSON(w, http.StatusOK, struct {
		Torrent string      `json:"torrent"`
		Files   []fileEntry `json:"files"`
	}{Torrent: src.Name(), Files: files})
}

func (s *Server) handleStatus(w http.ResponseWriter) {
	// Заметьте: 200, а не 503. Клиент отличает «метаданные ещё грузятся»
	// от «сервер недоступен» именно по успешному ответу с ready:false.
	// Пустая библиотека выглядит отсюда так же — специального состояния
	// «торрент не выбран» в контракте с телевизором нет.
	src, ok := s.source()
	if !ok {
		writeJSON(w, http.StatusOK, struct {
			Ready bool `json:"ready"`
		}{false})
		return
	}
	st := src.Stats()
	writeJSON(w, http.StatusOK, struct {
		Ready         bool          `json:"ready"`
		Peers         int           `json:"peers"`
		DownloadSpeed float64       `json:"downloadSpeed"`
		Downloaded    int64         `json:"downloaded"`
		Progress      float64       `json:"progress"`
		Playback      *hls.Snapshot `json:"playback"`
	}{
		Ready:         true,
		Peers:         st.Peers,
		DownloadSpeed: st.DownloadSpeed,
		Downloaded:    st.Downloaded,
		Progress:      st.Progress,
		Playback:      s.deps.HLS.ActiveSnapshot(),
	})
}

func (s *Server) handleStop(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, struct {
		OK      bool    `json:"ok"`
		Stopped *string `json:"stopped"`
	}{OK: true, Stopped: s.deps.HLS.Stop()})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request, rawIndex string) {
	src, ready := s.source()
	if !ready {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(src, rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}
	result, err := s.probe(r.Context(), src, index)
	if err != nil {
		// Текст ошибки уходит наружу дословно: браузерный клиент печатает
		// поле error как есть, и «ffprobe timed out after 25s» — это
		// осмысленное сообщение, в отличие от «Internal server error».
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request, rawIndex string) {
	src, ready := s.source()
	if !ready {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(src, rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}

	// Разбор идёт ВНЕ лока менеджера: он может занять до 25 секунд, и держать
	// на нём весь /api/status было бы худшим решением из возможных.
	meta, err := s.probe(r.Context(), src, index)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	q := r.URL.Query()
	subPref := q.Get("sub")
	if subPref == "" {
		subPref = "off"
	}
	// Полный ECMAScript ToNumber, а не ParseFloat: параметр приходит от клиента,
	// и «0x10», «1e3», «Infinity» здесь означают не то же, что в Go.
	start := jscompat.Or0(jscompat.ToNumber(q.Get("start")))
	if start < 0 {
		start = 0
	}

	snap, err := s.deps.HLS.Start(hls.StartOptions{
		Index:     index,
		Meta:      meta,
		AudioPref: q.Get("audio"),
		SubPref:   subPref,
		Start:     start,
	})
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handlePrebuffer запускает подкачку начала файла и отвечает немедленно.
func (s *Server) handlePrebuffer(w http.ResponseWriter, rawIndex string) {
	src, ready := s.source()
	if !ready {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(src, rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}
	s.startPrebuffer(src, index)
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// currentSource — источник без проверки готовности, для /raw.
func (s *Server) currentSource() mediasource.Source {
	if s.deps.Library == nil {
		return nil
	}
	return s.deps.Library.Current()
}

// parseIndex — getFile(): индекс должен быть целым и попадать в границы.
//
// jscompat.ToNumber, а не Atoi: «99999999999999999999» в JS это конечное
// целое 1e20, которое просто не проходит проверку границ и даёт 404,
// тогда как Atoi вернул бы ошибку переполнения.
func (s *Server) parseIndex(src mediasource.Source, raw string) (int, bool) {
	n := jscompat.ToNumber(raw)
	if !jscompat.IsInteger(n) || n < 0 {
		return 0, false
	}
	if int(n) >= len(sourceFiles(src)) {
		return 0, false
	}
	return int(n), true
}

// sourceFiles — список файлов источника, пустой при отсутствии торрента.
func sourceFiles(src mediasource.Source) []mediasource.File {
	if src == nil {
		return nil
	}
	return src.Files()
}

// videoFiles — фильтр серий из videoFiles(). Порядок НЕ меняется:
// телевизор хранит позиции просмотра по индексу файла в торренте.
func (s *Server) videoFiles(src mediasource.Source) []mediasource.File {
	all := sourceFiles(src)
	out := make([]mediasource.File, 0, len(all))
	for _, f := range all {
		if media.IsVideoName(f.Name) {
			out = append(out, f)
		}
	}
	return out
}

// neighbours — nextVideoIndex/prevVideoIndex: позиция в ОТФИЛЬТРОВАННОМ списке
// видео, а наружу отдаётся абсолютный индекс файла в торренте.
func (s *Server) neighbours(src mediasource.Source, index int) (next, prev *int) {
	list := s.videoFiles(src)
	pos := -1
	for i, f := range list {
		if f.Index == index {
			pos = i
			break
		}
	}
	if pos >= 0 && pos+1 < len(list) {
		v := list[pos+1].Index
		next = &v
	}
	if pos > 0 {
		v := list[pos-1].Index
		prev = &v
	}
	return next, prev
}

func (s *Server) probe(ctx context.Context, src mediasource.Source, index int) (*media.Result, error) {
	file, ok := s.file(src, index)
	if !ok {
		return nil, errFileNotFound
	}
	next, prev := s.neighbours(src, index)
	return s.deps.Prober.Probe(ctx, media.Request{
		Index: index, Name: file.Name, Next: next, Prev: prev,
		// Файлы субтитров ищутся по имени серии, а не по индексу: индекс
		// принадлежит торренту и после переключения означает другой файл.
		External: s.deps.Subs.TracksFor(file.Name),
	})
}

func (s *Server) file(src mediasource.Source, index int) (mediasource.File, bool) {
	files := sourceFiles(src)
	if index < 0 || index >= len(files) {
		return mediasource.File{}, false
	}
	return files[index], true
}

// startPrebuffer тянет первые 8 МБ файла, чтобы плеер стартовал быстрее.
//
// Контекст берётся ЖИЗНЕННЫЙ, а не запросный: /api/prebuffer отвечает
// {"ok":true} немедленно, контекст запроса отменяется тут же, и с ним
// подкачка стала бы тихим no-op. Прерывается такое чтение сменой торрента:
// закрытый Source будит своих читателей сам.
//
// Ключ занятости включает id торрента: после переключения тот же индекс
// означает уже другой файл, и подкачку для него блокировать нечем.
func (s *Server) startPrebuffer(src mediasource.Source, index int) {
	key := s.deps.Library.ActiveID() + "/" + strconv.Itoa(index)

	s.prebufMu.Lock()
	if _, busy := s.prebuf[key]; busy {
		s.prebufMu.Unlock()
		return
	}
	s.prebuf[key] = struct{}{}
	s.prebufMu.Unlock()

	go func() {
		defer func() {
			s.prebufMu.Lock()
			delete(s.prebuf, key)
			s.prebufMu.Unlock()
		}()

		reader, err := src.Open(index)
		if err != nil {
			log.Printf("Prebuffer [%d] failed: %v", index, err)
			return
		}
		defer reader.Close()

		file, _ := s.file(src, index)
		want := int64(prebufferBytes)
		if file.Length < want {
			want = file.Length
		}
		n, err := io.CopyN(io.Discard, mediasource.WithContext(s.baseCtx(), reader), want)
		if err != nil {
			log.Printf("Prebuffer [%d] failed after %d bytes: %v", index, n, err)
			return
		}
		log.Printf("Prebuffered [%d] %d MB", index, (n+(1<<19))/(1<<20))
	}()
}

func (s *Server) baseCtx() context.Context {
	if s.deps.BaseCtx != nil {
		return s.deps.BaseCtx
	}
	return context.Background()
}

var errFileNotFound = errors.New("File not found")
