package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/mediasource"
)

// rangeHeader — /^bytes=(\d*)-(\d*)$/ дословно.
//
// Разбирается вручную, а не через http.ParseRange, потому что расхождения
// пришлись бы ровно на те случаи, которые генерирует ffmpeg: «bytes=-» здесь
// означает весь файл с кодом 206, а ParseRange такое отвергает.
var rangeHeader = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

// rawContentType повторяет webtorrent-овский file.type (mime.getType по имени).
//
// Таблица зашита намеренно: mime.TypeByExtension читает системный mime.types,
// и на маке при разработке он один, а в образе другой — Content-Type начал бы
// зависеть от того, где собрали.
func rawContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4":
		return "video/mp4"
	case ".m4v":
		return "video/x-m4v"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	default:
		return "application/octet-stream"
	}
}

// serveRaw отдаёт файл торрента по HTTP. Через этот же эндпоинт ffmpeg
// и ffprobe читают торрент петлёй на 127.0.0.1.
func (s *Server) serveRaw(w http.ResponseWriter, r *http.Request, src mediasource.Source, index int) {
	file, ok := s.file(src, index)
	if !ok {
		writeText(w, http.StatusNotFound, "File not found", contentTypeText)
		return
	}
	size := file.Length

	rangeValue := r.Header.Get("Range")
	if rangeValue == "" {
		// Внимание: здесь НЕТ Access-Control-Allow-Origin, и это не упущение,
		// а поведение оригинала. /raw телевизор не читает — читают ffmpeg
		// и браузерный клиент с того же origin.
		h := w.Header()
		h.Set("Content-Type", rawContentType(file.Name))
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		h.Set("Accept-Ranges", "bytes")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		s.streamRange(w, r, src, index, 0, size-1)
		return
	}

	start, end, ok := parseRange(rangeValue, size)
	if !ok {
		// 416 несёт ТОЛЬКО Content-Range: ни тела, ни Content-Type, ни CORS.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	h := w.Header()
	h.Set("Content-Type", rawContentType(file.Name))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	s.streamRange(w, r, src, index, start, end)
}

// parseRange разбирает заголовок Range по правилам оригинала.
//
// Краевые случаи, которые здесь важнее всего и все проверены тестами:
//   - "bytes=-"   обе части пустые -> это НЕ suffix-форма, а весь файл (206)
//   - "bytes=-0"  suffix ноль -> start=size, end=size-1 -> end<start -> 416
//   - "bytes=0-1,3-4" множественный диапазон -> шаблон не совпал -> 416
func parseRange(value string, size int64) (start, end int64, ok bool) {
	m := rangeHeader.FindStringSubmatch(value)
	if m == nil {
		return 0, 0, false
	}

	if m[1] == "" && m[2] != "" {
		suffix := int64(jscompat.ToNumber(m[2]))
		start = size - suffix
		if start < 0 {
			start = 0
		}
		end = size - 1
	} else {
		if m[1] != "" {
			start = int64(jscompat.ToNumber(m[1]))
		}
		end = size - 1
		if m[2] != "" {
			end = int64(jscompat.ToNumber(m[2]))
		}
	}

	if start < 0 || end < start || start >= size {
		return 0, 0, false
	}
	if end > size-1 {
		end = size - 1
	}
	return start, end, true
}

// streamRange копирует нужный кусок файла в ответ.
//
// Reader закрывается обязательно: пока он жив, вокруг его позиции держится
// окно приоритета, и торрент продолжает качаться уже после ухода клиента.
// В Node ту же работу делал res.on('close', () => stream.destroy()).
func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, src mediasource.Source, index int, start, end int64) {
	reader, err := src.Open(index)
	if err != nil {
		return
	}
	defer reader.Close()

	if start > 0 {
		if _, err := reader.Seek(start, io.SeekStart); err != nil {
			return
		}
	}
	// Чтение привязано к контексту запроса: на раздаче без пиров оно иначе
	// висело бы вечно, удерживая и горутину, и окно приоритета.
	_, _ = io.CopyN(w, mediasource.WithContext(r.Context(), reader), end-start+1)
}
