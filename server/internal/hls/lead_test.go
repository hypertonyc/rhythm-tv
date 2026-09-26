package hls

import (
	"os"
	"path/filepath"
	"testing"
)

func transcodingSession(duration, start float64) *Session {
	return &Session{videoMode: "transcode", duration: duration, start: start, progress: &progressWriter{}}
}

func TestStartupSegmentsCopyIsAlwaysTwo(t *testing.T) {
	s := transcodingSession(1500, 0)
	s.videoMode = "copy"
	if got := startupSegmentsFor(s, 1000); got != StartupSegments {
		t.Errorf("copy ждёт %d сегментов, ожидалось %d", got, StartupSegments)
	}
}

// Подобранный после выкатки сеанс своего ffmpeg не имеет, и мерить там нечего.
func TestStartupSegmentsAdoptedIsTwo(t *testing.T) {
	s := transcodingSession(1500, 0)
	s.progress = nil
	if got := startupSegmentsFor(s, 1000); got != StartupSegments {
		t.Errorf("подобранный сеанс ждёт %d сегментов", got)
	}
}

func TestStartupSegmentsWaitsForMeasurement(t *testing.T) {
	s := transcodingSession(1500, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 2, 1000, 1
	if got := startupSegmentsFor(s, 5000); got != StartupSegments+1 {
		t.Errorf("без замера отпускаем на %d сегментах, ожидалось %d", got, StartupSegments+1)
	}
}

func TestStartupSegmentsFastTranscodeKeepsTwo(t *testing.T) {
	s := transcodingSession(1500, 0)
	// 4 интервала по 4 с за 8 с — вдвое быстрее реального времени.
	s.segments, s.firstOutputAt, s.firstOutputSegs = 5, 1000, 1
	if got := startupSegmentsFor(s, 9000); got != StartupSegments {
		t.Errorf("быстрое перекодирование ждёт %d сегментов", got)
	}
}

// Замер 26.09.2026: 4K HDR со звуком и субтитрами на ffmpeg 5.1 — ~0.9×.
func TestStartupSegmentsSlowTranscodeBuildsLead(t *testing.T) {
	s := transcodingSession(1502, 52)
	// 9 интервалов по 4 с за 40 с — 0.9×.
	s.segments, s.firstOutputAt, s.firstOutputSegs = 10, 1000, 1
	rate, ok := outputRate(s, 41000)
	if !ok || rate != 0.9 {
		t.Fatalf("скорость %v %v, ожидалось 0.9", rate, ok)
	}
	// (1502−52)·(1 − 0.9/1.05) = 207.1 с → 52 сегмента.
	if got := startupSegmentsFor(s, 41000); got != 52 {
		t.Errorf("запас %d сегментов, ожидалось 52", got)
	}
}

func TestStartupSegmentsLeadIsCapped(t *testing.T) {
	s := transcodingSession(3000, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 3, 1000, 1
	if got := startupSegmentsFor(s, 17000); got != maxLeadSeconds/SegmentSeconds {
		t.Errorf("запас %d сегментов, потолок %d", got, maxLeadSeconds/SegmentSeconds)
	}
}

// Время первого сегмента запоминается один раз: иначе в замер попало бы
// декодирование с начала серии, которое при перемотке идёт до первого сегмента.
func TestPollSegmentsRemembersFirstOutput(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dir: dir}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pollSegments(s, 1000)
	if s.firstOutputAt != 0 {
		t.Fatalf("время первого сегмента без сегментов: %d", s.firstOutputAt)
	}
	write("seg00000.ts")
	pollSegments(s, 82000)
	write("seg00001.ts")
	pollSegments(s, 86000)
	if s.firstOutputAt != 82000 || s.firstOutputSegs != 1 {
		t.Errorf("первый сегмент: at=%d segs=%d, ожидалось 82000/1", s.firstOutputAt, s.firstOutputSegs)
	}
}
