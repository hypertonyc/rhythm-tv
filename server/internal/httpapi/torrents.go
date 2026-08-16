package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/avdav/torrent-media/server/internal/library"
)

// Ручки библиотеки торрентов.
//
// Ни одной из них нет в Node-эталоне, и телевизор в них не ходит: список серий
// он по-прежнему берёт из /api/files, просто теперь этот список принадлежит
// тому торренту, который выбрали с телефона. Поэтому здесь можно то, чего
// нельзя в остальном API: проверять метод и отвечать кодами, которых оригинал
// не отдавал.
//
// Своей авторизации тут нет — её нет у сервера вообще, наружу он выставляется
// reverse-proxy с токеном в пути. Загрузка .torrent доступна тому же, кто уже
// может запустить перекодирование, так что новой границы доверия не возникает;
// ограничен только размер (library.MaxTorrentBytes).

// handleTorrents: GET — список, POST — загрузка .torrent.
func (s *Server) handleTorrents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.writeLibrary(w, http.StatusOK)
	case http.MethodPost:
		s.handleTorrentUpload(w, r)
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "Use GET to list or POST to upload")
	}
}

// handleTorrentUpload принимает .torrent телом запроса или полем формы.
//
// Два способа не от щедрости: телефонный клиент шлёт файл телом (XHR умеет
// отправить File как есть), а multipart нужен обычной HTML-форме и curl -F.
func (s *Server) handleTorrentUpload(w http.ResponseWriter, r *http.Request) {
	body, cleanup, err := uploadBody(r)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	entry, err := s.deps.Library.Add(body)
	if err != nil {
		errorJSON(w, torrentErrorStatus(err), err.Error())
		return
	}
	log.Printf("library: added %q (%s)", entry.Name, entry.ID)

	// Первый торрент в пустой библиотеке включается сам: активировать нечего
	// было, прервать нечего, а лишний тык на телефоне ни к чему. Когда что-то
	// уже активно, выбор остаётся за человеком — иначе загрузка на телефоне
	// оборвала бы серию, которую в этот момент смотрят.
	if s.deps.Library.ActiveID() == "" {
		if activated, err := s.deps.Library.Activate(entry.ID); err != nil {
			log.Printf("library: не удалось включить %q: %v", entry.Name, err)
		} else {
			entry = activated
		}
	}

	s.writeLibrary(w, http.StatusCreated)
}

// handleTorrentAction — /api/torrents/<id>/{activate,delete}.
func (s *Server) handleTorrentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "Use POST")
		return
	}

	switch action {
	case "activate":
		if id == s.deps.Library.ActiveID() {
			s.writeLibrary(w, http.StatusOK)
			return
		}
		// Сеанс гасится ДО переключения: ffmpeg читает файл прежнего торрента
		// петлёй через /raw, и оставить его в живых значило бы перекодировать
		// то, чего в активном торренте уже нет.
		if stopped := s.deps.HLS.Stop(); stopped != nil {
			log.Printf("library: stopped session %s before switching torrent", *stopped)
		}
		entry, err := s.deps.Library.Activate(id)
		if err != nil {
			errorJSON(w, torrentErrorStatus(err), err.Error())
			return
		}
		log.Printf("library: active torrent is now %q (%s)", entry.Name, entry.ID)
		s.writeLibrary(w, http.StatusOK)

	case "delete":
		// withData удаляет ещё и скачанное — десятки гигабайт, обратно только
		// закачкой. Поэтому по умолчанию выключено и включается явным ?data=1.
		withData := r.URL.Query().Get("data") == "1"
		if id == s.deps.Library.ActiveID() {
			if stopped := s.deps.HLS.Stop(); stopped != nil {
				log.Printf("library: stopped session %s before removing torrent", *stopped)
			}
		}
		if err := s.deps.Library.Remove(id, withData); err != nil {
			errorJSON(w, torrentErrorStatus(err), err.Error())
			return
		}
		log.Printf("library: removed %s (данные %s)", id,
			map[bool]string{true: "удалены", false: "оставлены"}[withData])
		s.writeLibrary(w, http.StatusOK)

	default:
		writeText(w, http.StatusNotFound, "Not found", contentTypeText)
	}
}

// writeLibrary отдаёт состояние библиотеки целиком.
//
// Каждая мутация отвечает полным списком, а не одной записью: клиенту тогда
// не нужно догадываться, что ещё изменилось (с какого торрента снялся флаг
// активного, каким именем лёг файл), и второй запрос за списком не нужен.
func (s *Server) writeLibrary(w http.ResponseWriter, status int) {
	entries, err := s.deps.Library.List()
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		// Пустой слайс маршалится в null, а клиент читает .length.
		entries = make([]library.Entry, 0)
	}

	// Указатель, потому что клиент сравнивает с null строго, как и в остальном
	// API: отсутствующий ключ дал бы undefined.
	var active *string
	if id := s.deps.Library.ActiveID(); id != "" {
		active = &id
	}

	writeJSON(w, status, struct {
		Active   *string         `json:"active"`
		Torrents []library.Entry `json:"torrents"`
	}{Active: active, Torrents: entries})
}

// uploadBody достаёт содержимое .torrent из запроса.
func uploadBody(r *http.Request) (io.Reader, func(), error) {
	// Тело ограничивается до разбора: multipart-форма иначе целиком уехала
	// бы во временный файл ещё до того, как мы посмотрели на её размер.
	r.Body = http.MaxBytesReader(nil, r.Body, library.MaxTorrentBytes+1)

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return r.Body, func() {}, nil
	}

	file, _, err := r.FormFile("torrent")
	if err != nil {
		return nil, nil, errors.New("ожидалось поле формы torrent с .torrent-файлом")
	}
	return file, func() { file.Close() }, nil
}

// torrentErrorStatus переводит ошибки библиотеки в коды ответа.
func torrentErrorStatus(err error) int {
	switch {
	case errors.Is(err, library.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, library.ErrBadTorrent):
		return http.StatusBadRequest
	case errors.Is(err, library.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}
