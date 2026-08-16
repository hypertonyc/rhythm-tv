package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/httpapi/web"
	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/mediasource"
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
)

const prebufferBytes = 8 << 20

// Deps — всё, что нужно обработчикам.
type Deps struct {
	// MULTI-TORRENT PIN: здесь появится не второй Source, а резолвер
	// вида func(*http.Request) (Source, bool); сигнатуры обработчиков
	// от этого не изменятся — они уже берут источник локальной переменной.
	Source mediasource.Source
	Prober *media.Prober
	HLS    *hls.Manager
	// BaseCtx живёт столько же, сколько процесс. Нужен префетчу: контекст
	// запроса там не годится (см. handlePrebuffer).
	BaseCtx context.Context
}

// Server — обработчик всех маршрутов.
type Server struct {
	deps Deps

	prebufMu sync.Mutex
	prebuf   map[int]struct{}
}

func New(deps Deps) *Server {
	return &Server{deps: deps, prebuf: make(map[int]struct{})}
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
		index, ok := s.parseIndex(m[1])
		if !ok {
			writeText(w, http.StatusNotFound, "File not found", contentTypeText)
			return
		}
		s.serveRaw(w, r, index)
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
	if !s.deps.Source.Ready() {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	list := s.videoFiles()
	files := make([]fileEntry, 0, len(list))
	for _, f := range list {
		files = append(files, fileEntry{Index: f.Index, Name: f.Name, Length: f.Length})
	}
	writeJSON(w, http.StatusOK, struct {
		Torrent string      `json:"torrent"`
		Files   []fileEntry `json:"files"`
	}{Torrent: s.deps.Source.Name(), Files: files})
}

func (s *Server) handleStatus(w http.ResponseWriter) {
	// Заметьте: 200, а не 503. Клиент отличает «метаданные ещё грузятся»
	// от «сервер недоступен» именно по успешному ответу с ready:false.
	if !s.deps.Source.Ready() {
		writeJSON(w, http.StatusOK, struct {
			Ready bool `json:"ready"`
		}{false})
		return
	}
	st := s.deps.Source.Stats()
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
	if !s.deps.Source.Ready() {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}
	result, err := s.probe(r.Context(), index)
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
	if !s.deps.Source.Ready() {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}

	// Разбор идёт ВНЕ лока менеджера: он может занять до 25 секунд, и держать
	// на нём весь /api/status было бы худшим решением из возможных.
	meta, err := s.probe(r.Context(), index)
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
	if !s.deps.Source.Ready() {
		errorJSON(w, http.StatusServiceUnavailable, "Torrent metadata is loading")
		return
	}
	index, ok := s.parseIndex(rawIndex)
	if !ok {
		errorJSON(w, http.StatusNotFound, "File not found")
		return
	}
	s.startPrebuffer(index)
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// parseIndex — getFile(): индекс должен быть целым и попадать в границы.
//
// jscompat.ToNumber, а не Atoi: «99999999999999999999» в JS это конечное
// целое 1e20, которое просто не проходит проверку границ и даёт 404,
// тогда как Atoi вернул бы ошибку переполнения.
func (s *Server) parseIndex(raw string) (int, bool) {
	n := jscompat.ToNumber(raw)
	if !jscompat.IsInteger(n) || n < 0 {
		return 0, false
	}
	files := s.deps.Source.Files()
	if int(n) >= len(files) {
		return 0, false
	}
	return int(n), true
}

// videoFiles — фильтр серий из videoFiles(). Порядок НЕ меняется:
// телевизор хранит позиции просмотра по индексу файла в торренте.
func (s *Server) videoFiles() []mediasource.File {
	all := s.deps.Source.Files()
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
func (s *Server) neighbours(index int) (next, prev *int) {
	list := s.videoFiles()
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

func (s *Server) probe(ctx context.Context, index int) (*media.Result, error) {
	file, ok := s.file(index)
	if !ok {
		return nil, errFileNotFound
	}
	next, prev := s.neighbours(index)
	return s.deps.Prober.Probe(ctx, media.Request{
		Index: index, Name: file.Name, Next: next, Prev: prev,
	})
}

func (s *Server) file(index int) (mediasource.File, bool) {
	files := s.deps.Source.Files()
	if index < 0 || index >= len(files) {
		return mediasource.File{}, false
	}
	return files[index], true
}

// startPrebuffer тянет первые 8 МБ файла, чтобы плеер стартовал быстрее.
//
// Контекст берётся ЖИЗНЕННЫЙ, а не запросный: /api/prebuffer отвечает
// {"ok":true} немедленно, контекст запроса отменяется тут же, и с ним
// подкачка стала бы тихим no-op.
func (s *Server) startPrebuffer(index int) {
	s.prebufMu.Lock()
	if _, busy := s.prebuf[index]; busy {
		s.prebufMu.Unlock()
		return
	}
	s.prebuf[index] = struct{}{}
	s.prebufMu.Unlock()

	go func() {
		defer func() {
			s.prebufMu.Lock()
			delete(s.prebuf, index)
			s.prebufMu.Unlock()
		}()

		reader, err := s.deps.Source.Open(index)
		if err != nil {
			log.Printf("Prebuffer [%d] failed: %v", index, err)
			return
		}
		defer reader.Close()

		file, _ := s.file(index)
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
