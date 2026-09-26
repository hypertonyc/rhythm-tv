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
	if got := startupSegmentsFor(s, 1000, 0); got != StartupSegments {
		t.Errorf("copy ждёт %d сегментов, ожидалось %d", got, StartupSegments)
	}
}

// Подобранный после выкатки сеанс своего ffmpeg не имеет, и мерить там нечего.
func TestStartupSegmentsAdoptedIsTwo(t *testing.T) {
	s := transcodingSession(1500, 0)
	s.progress = nil
	if got := startupSegmentsFor(s, 1000, 0); got != StartupSegments {
		t.Errorf("подобранный сеанс ждёт %d сегментов", got)
	}
}

func TestStartupSegmentsWaitsForMeasurement(t *testing.T) {
	s := transcodingSession(1500, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 2, 1000, 1
	if got := startupSegmentsFor(s, 5000, 0); got != StartupSegments+1 {
		t.Errorf("без замера отпускаем на %d сегментах, ожидалось %d", got, StartupSegments+1)
	}
}

func TestStartupSegmentsFastTranscodeKeepsTwo(t *testing.T) {
	s := transcodingSession(1500, 0)
	// 4 интервала по 4 с за 8 с — вдвое быстрее реального времени.
	s.segments, s.firstOutputAt, s.firstOutputSegs = 5, 1000, 1
	if got := startupSegmentsFor(s, 9000, 0); got != StartupSegments {
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
	if got := startupSegmentsFor(s, 41000, 0); got != 52 {
		t.Errorf("запас %d сегментов, ожидалось 52", got)
	}
}

func TestStartupSegmentsLeadIsCapped(t *testing.T) {
	s := transcodingSession(3000, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 3, 1000, 1
	if got := startupSegmentsFor(s, 17000, 0); got != maxLeadSeconds/SegmentSeconds {
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

// Начало серии быстрее середины: при обычных для формата 0.9× замер 1.1×
// в первые секунды не должен отпускать телевизор на двух сегментах.
func TestStartupSegmentsTakesTheSlowerOfMeasuredAndUsual(t *testing.T) {
	s := transcodingSession(1502, 52)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 12, 1000, 1
	if rate, _ := outputRate(s, 41000); rate != 1.1 {
		t.Fatalf("замер %v, ожидалось 1.1", rate)
	}
	if got := startupSegmentsFor(s, 41000, 0.9); got != 52 {
		t.Errorf("запас %d сегментов, ожидалось 52 — как при 0.9×", got)
	}
}

// Обычная скорость известна до первого сегмента, и ждать замера тогда незачем.
func TestStartupSegmentsUsesUsualBeforeMeasurement(t *testing.T) {
	s := transcodingSession(1502, 52)
	if got := startupSegmentsFor(s, 1000, 0.9); got != 52 {
		t.Errorf("запас %d сегментов, ожидалось 52", got)
	}
}

func finishedSession(produced, elapsedMs int) *Session {
	s := transcodingSession(1502, 0)
	s.pipeline = Pipeline{Video: PipelineTrack{From: "hevc 3840x2160 HDR PQ", To: "h264 1280x720"}}
	s.firstOutputAt, s.firstOutputSegs = 1000, 1
	s.segments = 1 + produced/SegmentSeconds
	last := int64(1000 + elapsedMs)
	s.lastOutputAt = &last
	return s
}

func TestRememberRateSmoothsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tms-rates")
	m := &Manager{RatesPath: path}

	m.rememberRateLocked(finishedSession(400, 500000))
	m.rememberRateLocked(finishedSession(400, 400000))
	key := rateKey(finishedSession(400, 1))
	if got := m.rates[key]; got != 0.9 {
		t.Fatalf("обычная скорость %v, ожидалось (0.8+1.0)/2 = 0.9", got)
	}

	fresh := &Manager{RatesPath: path}
	if got := fresh.usualRateLocked(finishedSession(400, 1)); got != 0.9 {
		t.Errorf("после перезапуска обычная скорость %v, ожидалось 0.9", got)
	}
}

func TestRememberRateSkipsShortAndCopiedSessions(t *testing.T) {
	m := &Manager{}
	m.rememberRateLocked(finishedSession(40, 40000))
	copied := finishedSession(400, 20000)
	copied.videoMode = "copy"
	m.rememberRateLocked(copied)
	if len(m.rates) != 0 {
		t.Errorf("запомнено лишнее: %v", m.rates)
	}
}

// Живой сеанс 26.09.2026 на ffmpeg 7.1: 26 сегментов, а в /api/pipeline
// encodedMs и speed пустые — ffmpeg с редкими встроенными субтитрами писал N/A.
func TestProgressFallsBackToSegmentsWhenFFmpegSaysNA(t *testing.T) {
	now := int64(101000)
	m := &Manager{NowMilli: func() int64 { return now }}
	s := transcodingSession(1502, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 26, 1000, 1
	if _, err := s.progress.Write([]byte("out_time=N/A\nspeed=N/A\nprogress=continue\n")); err != nil {
		t.Fatal(err)
	}

	p := m.progressLocked(s)
	if p.EncodedMs == nil || *p.EncodedMs != 104000 {
		t.Errorf("encodedMs = %v, ожидалось 26 сегментов = 104000", p.EncodedMs)
	}
	if p.Speed == nil || *p.Speed != 1.0 {
		t.Errorf("speed = %v, ожидалось 25·4 с за 100 с = 1.0", p.Speed)
	}
}

func TestProgressKeepsFFmpegTimeWhenItIsAhead(t *testing.T) {
	m := &Manager{NowMilli: func() int64 { return 101000 }}
	s := transcodingSession(1502, 0)
	s.segments, s.firstOutputAt, s.firstOutputSegs = 2, 1000, 1
	if _, err := s.progress.Write([]byte("out_time=00:00:11.500000\nspeed=1.2x\nprogress=continue\n")); err != nil {
		t.Fatal(err)
	}
	p := m.progressLocked(s)
	if p.EncodedMs == nil || *p.EncodedMs != 11500 {
		t.Errorf("encodedMs = %v, ожидалось 11500 от ffmpeg", p.EncodedMs)
	}
	if p.Speed == nil || *p.Speed != 1.2 {
		t.Errorf("speed = %v, ожидалось 1.2 от ffmpeg", p.Speed)
	}
}
