package httpapi

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestClientLogWritesOneLine(t *testing.T) {
	buf := captureLog(t)
	s, _, _ := newTestServer(t, nil, "")

	rec := do(s, "POST", "/api/client-log", strings.NewReader("ev=beat up=60\n2026/11/25 10:00:00 чистка места: подделка\r\x00 mode=player"))
	if rec.Code != 200 {
		t.Fatalf("код %d", rec.Code)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("тело телевизора обязано лечь одной строкой:\n%q", out)
	}
	if !strings.Contains(out, "ТВ: ev=beat up=60 2026/11/25 10:00:00 чистка места: подделка mode=player") {
		t.Errorf("строка не та:\n%q", out)
	}
}

func TestClientLogCapsBody(t *testing.T) {
	buf := captureLog(t)
	s, _, _ := newTestServer(t, nil, "")

	do(s, "POST", "/api/client-log", strings.NewReader("ev=beat "+strings.Repeat("я", clientLogMaxBytes)))
	if n := len(buf.String()); n > clientLogMaxBytes+100 {
		t.Errorf("в лог ушло %d байт, предел тела %d", n, clientLogMaxBytes)
	}
}

func TestClientLogRejectsEmptyBody(t *testing.T) {
	buf := captureLog(t)
	s, _, _ := newTestServer(t, nil, "")

	if rec := do(s, "POST", "/api/client-log", strings.NewReader(" \n\t")); rec.Code != 400 {
		t.Errorf("код %d, ожидался 400", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("пустое тело попало в лог: %q", buf.String())
	}
}
