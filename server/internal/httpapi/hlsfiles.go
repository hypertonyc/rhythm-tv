package httpapi

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// hlsFileName — /^[A-Za-z0-9._-]+\.(m3u8|ts|vtt)$/ БЕЗ флага /i.
//
// Регистр здесь важен и расходится с маршрутом намеренно: маршрут в роутере
// регистронезависим, поэтому «index.M3U8» до обработчика доходит, а вот эта
// проверка его отвергает с кодом 400. Асимметрия унаследована от оригинала.
var hlsFileName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(m3u8|ts|vtt)$`)

// serveHLSFile отдаёт файл из каталога сеанса.
//
// Range здесь НЕ поддерживается сознательно: оригинал отвечал только 200
// и целым файлом. Поэтому http.ServeContent/ServeFile не годятся — они добавят
// Accept-Ranges и ETag и начнут честно отдавать 206, а AVPlay на телевизоре
// вполне способен прислать Range на .ts, и его поведение изменится.
func (s *Server) serveHLSFile(w http.ResponseWriter, r *http.Request, sessionID, fileName string) {
	dir, ok := s.deps.HLS.SessionDir(sessionID)
	if !ok {
		writeText(w, http.StatusNotFound, "HLS session not found", contentTypeText)
		return
	}
	if !hlsFileName.MatchString(fileName) {
		writeText(w, http.StatusBadRequest, "Bad HLS path", contentTypeText)
		return
	}

	// Тип выбирается по СТРОЧНОМУ суффиксу, как endsWith в оригинале.
	contentType := "video/mp2t"
	cacheControl := "public, max-age=3600"
	switch {
	case strings.HasSuffix(fileName, ".m3u8"):
		contentType = "application/vnd.apple.mpegurl"
		// Плейлист дописывается по мере появления сегментов, кэшировать нельзя.
		cacheControl = "no-store"
	case strings.HasSuffix(fileName, ".vtt"):
		contentType = "text/vtt; charset=utf-8"
	}

	full := filepath.Join(dir, fileName)
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
		return
	}

	f, err := os.Open(full)
	if err != nil {
		writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	h.Set("Cache-Control", cacheControl)
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}
