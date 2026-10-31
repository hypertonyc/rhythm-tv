package mediasource

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSlogDropsPartFileFlushNoise(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slogCancelFilter{next: slog.NewTextHandler(&buf, nil)})

	l.Warn("error flushing file before promotion",
		"file", "/store/x/Eng Subs/s01e02.srt.part", "err", "open /store/x/Eng Subs/s01e02.srt.part: no such file or directory")
	l.Warn("file too small for piece marked as complete", "file", "/store/x/s01e03.avi")

	out := buf.String()
	if strings.Contains(out, "promotion") {
		t.Errorf("шум про .part попал в лог:\n%s", out)
	}
	// Это предупреждение и есть фантом из phantom.go — его глушить нельзя.
	if !strings.Contains(out, "file too small for piece marked as complete") {
		t.Errorf("фильтр съел соседнее предупреждение:\n%s", out)
	}
}
