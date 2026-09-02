package mediasource

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

// Обработчик зовётся на каждый неудачный кусок, а на полном диске это десятки
// раз в секунду: 02.09.2026 в логе прода от умолчания anacrolix осталась
// сплошная стена «no space left on device». Поэтому проверяется не только то,
// что след в логе есть, но и то, что он один.
func TestWriteChunkErrorLogsOncePerWindow(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())

	handle := onWriteChunkError("Big Bang Theory")
	err := errors.New("no space left on device")
	for i := 0; i < 5; i++ {
		handle(err)
	}

	got := buf.String()
	if n := strings.Count(got, "no space left on device"); n != 1 {
		t.Fatalf("ожидалась одна строка в логе, получено %d:\n%s", n, got)
	}
	if !strings.Contains(got, "Big Bang Theory") {
		t.Fatalf("в логе нет имени торрента:\n%s", got)
	}
}
