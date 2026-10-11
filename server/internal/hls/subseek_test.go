package hls

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/media"
)

func probeWith(t *testing.T, output string) (*Manager, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffmpeg — shell-скрипт")
	}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out.vtt")
	argv := filepath.Join(tmp, "argv")
	if err := os.WriteFile(out, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(tmp, "ffmpeg-shim")
	script := "#!/bin/sh\necho \"$@\" > " + argv + "\ncat " + out + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{TmpDir: tmp, FFmpeg: shim}, argv
}

func TestProbeDetectsFfmpegThatIgnoresSeek(t *testing.T) {
	m, argv := probeWith(t, "WEBVTT\n\n00:03.000 --> 00:04.000\nx\n")
	if err := m.ProbeSubtitleSeek(); err != nil {
		t.Fatal(err)
	}
	if !m.ShiftsSubtitles() {
		t.Error("реплика осталась на 3-й секунде, а сервер не взялся сдвигать")
	}
	line, _ := os.ReadFile(argv)
	if i, ss := strings.Index(string(line), "-i "), strings.Index(string(line), "-ss 1"); i < 0 || ss < i {
		t.Errorf("проба обязана проверять output-side -ss, как в BuildArgs: %s", line)
	}
}

func TestProbeDetectsFixedFfmpeg(t *testing.T) {
	m, _ := probeWith(t, "WEBVTT\n\n00:02.000 --> 00:03.000\nx\n")
	if err := m.ProbeSubtitleSeek(); err != nil {
		t.Fatal(err)
	}
	if m.ShiftsSubtitles() {
		t.Error("ffmpeg сдвинул реплику сам, а сервер сдвинет её ещё раз")
	}
}

func TestProbeFailsOnUnexpectedOutput(t *testing.T) {
	m, _ := probeWith(t, "WEBVTT\n")
	if err := m.ProbeSubtitleSeek(); err == nil {
		t.Error("пустой вывод принят за ответ")
	}
	if m.ShiftsSubtitles() {
		t.Error("без ответа сервер не должен сдвигать метки")
	}
}

func embeddedEpisode() *media.Result {
	meta := copyableEpisode()
	meta.Subtitles = []media.SubtitleTrack{{Index: 3, Code: "rus", Label: "Russian", Codec: "mov_text"}}
	return meta
}

func TestEmbeddedSubtitlesShiftByAlignedStart(t *testing.T) {
	m, args, _ := seekProbe(t, 186.186, true)
	m.shiftSubtitles = true

	if _, err := m.Start(StartOptions{Index: 0, Meta: embeddedEpisode(), AudioPref: "eng", SubPref: "rus", Start: 195}); err != nil {
		t.Fatal(err)
	}
	line := args()
	if !strings.Contains(line, "-ss 186.086") || !strings.Contains(line, "-bsf:s setts=ts=TS-186.086/TB") {
		t.Errorf("субтитры обязаны сдвигаться на ту же секунду, что ушла в -ss:\n%s", line)
	}
}

func TestSubtitleShiftOnlyWhereFfmpegLeftEpisodeTime(t *testing.T) {
	embedded := &media.SubtitleTrack{Index: 3, Code: "rus", Codec: "mov_text"}
	external := &media.SubtitleTrack{Index: -1, Code: "rus", Codec: "srt", SourcePath: "/subs/s03e23.srt"}
	cases := []struct {
		name  string
		p     Params
		shift bool
	}{
		{"ffmpeg без исправления, перемотка", Params{Subtitle: embedded, Start: 186.086, ShiftSubtitles: true}, true},
		{"ffmpeg сдвигает сам", Params{Subtitle: embedded, Start: 186.086}, false},
		{"внешние сдвинуты при записи", Params{Subtitle: external, Start: 186.086, ShiftSubtitles: true}, false},
		{"старт с начала", Params{Subtitle: embedded, Start: 0, ShiftSubtitles: true}, false},
		{"без субтитров", Params{Start: 186.086, ShiftSubtitles: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.p.RawURL, c.p.Dir, c.p.Video = "u", "/tmp/d", copyableEpisode().Video
			line := strings.Join(BuildArgs(c.p), " ")
			if got := strings.Contains(line, "-bsf:s setts=ts=TS-186.086/TB"); got != c.shift {
				t.Errorf("сдвиг субтитров %v, ожидался %v:\n%s", got, c.shift, line)
			}
		})
	}
}
