package httpapi

import (
	"io"
	"log"
	"net/http"
	"strings"
	"unicode"
)

const clientLogMaxBytes = 2048

func (s *Server) handleClientLog(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, clientLogMaxBytes))
	line := clientLogLine(string(body))
	if line == "" {
		errorJSON(w, http.StatusBadRequest, "Empty client log line")
		return
	}
	log.Printf("ТВ: %s", line)
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// Тело пишет телевизор, а читаем мы его в логе построчно: перевод строки в нём подделал бы чужую запись.
func clientLogLine(body string) string {
	printable := strings.Map(func(r rune) rune {
		if r == unicode.ReplacementChar || !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, body)
	return strings.Join(strings.Fields(printable), " ")
}
