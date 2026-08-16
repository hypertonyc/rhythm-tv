package httpapi

import (
	"log"
	"net/http"
	"strconv"

	"github.com/avdav/torrent-media/server/internal/jscompat"
)

const (
	contentTypeJSON = "application/json; charset=utf-8"
	contentTypeText = "text/plain; charset=utf-8"
	contentTypeHTML = "text/html; charset=utf-8"
)

// writeJSON — sendJson() из server.mjs: четыре заголовка, ни одним больше.
//
// Content-Length ставится явно, потому что jscompat.Marshal уже дал готовые
// байты; полагаться на chunked нельзя — телевизор читает длину.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := jscompat.Marshal(v)
	if err != nil {
		// Сюда попадает только NaN/Inf: в JS это дало бы null, в Go — ошибку.
		// Гасить молча нельзя, но и разворачивать 500 из уже начатого ответа поздно.
		log.Printf("json marshal failed: %v", err)
		writeText(w, http.StatusInternalServerError, "Internal server error", contentTypeText)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentTypeJSON)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeText — sendText(). Тем же набором заголовков отдаётся и «/» с HTML.
func writeText(w http.ResponseWriter, status int, text, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(text)))
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

// errorJSON — {"error": "..."} с нужным кодом.
func errorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
