package hls

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/avdav/torrent-media/server/internal/subs"
)

// ffmpeg 6.1–9.0 не применяет output-side -ss к пакетам субтитров (исправлено в апстриме 7f2b4adeac,
// 27.09.2026): реплики остаются во времени серии, а видео идёт от нуля. Проверяется поведение, а не версия.
func (m *Manager) ProbeSubtitleSeek() error {
	f, err := os.CreateTemp(m.tmpDir(), "tms-subseek-*.srt")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("1\n00:00:03,000 --> 00:00:04,000\nx\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.ffmpeg(), "-hide_banner", "-loglevel", "error",
		"-i", f.Name(), "-ss", "1", "-c:s", "webvtt", "-f", "webvtt", "-").Output()
	if err != nil {
		return err
	}

	start, ok := subs.FirstCueStart(out)
	switch {
	case !ok:
		return fmt.Errorf("в выводе ffmpeg нет реплики: %q", out)
	case math.Abs(start-3) < 0.01:
		m.shiftSubtitles = true
		return nil
	case math.Abs(start-2) < 0.01:
		m.shiftSubtitles = false
		return nil
	}
	return fmt.Errorf("реплика на %.3f с вместо 2 или 3", start)
}

func (m *Manager) ShiftsSubtitles() bool {
	return m.shiftSubtitles
}
