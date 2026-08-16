package hls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/subs"
)

const externalSRT = "1\n00:00:10,000 --> 00:00:12,000\nПривет\n"

// TestBuildArgsIgnoresExternalSubtitle — внешнюю дорожку ffmpeg мапить нечем:
// потока с таким номером во входном файле нет. Вторым входом её тоже
// не подсунуть — при output-side -ss ffmpeg выбрасывает ранние реплики,
// но оставшимся время не пересчитывает.
func TestBuildArgsIgnoresExternalSubtitle(t *testing.T) {
	args := BuildArgs(Params{
		RawURL:     "http://127.0.0.1:8000/raw/7",
		Dir:        "/tmp/tms-hls-x",
		VideoIndex: 0,
		Audio:      &media.AudioTrack{Index: 1, Code: "eng"},
		Subtitle: &media.SubtitleTrack{
			Index: -1, Code: "rus", Label: "Russian", SourcePath: "/subs/s01e01.srt",
		},
		Start: 991.52,
	})

	line := strings.Join(args, " ")
	for _, forbidden := range []string{"/subs/s01e01.srt", "-c:s", "-var_stream_map", "-master_pl_name", "-map 0:-1"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("в argv попало %q:\n%s", forbidden, line)
		}
	}
	if !strings.Contains(line, " -sn ") {
		t.Errorf("нет -sn:\n%s", line)
	}
	// Ровно один -i: второй вход не появился.
	if n := strings.Count(line, " -i "); n != 1 {
		t.Errorf("входов %d, ожидался один:\n%s", n, line)
	}
}

// TestStartWritesExternalSubtitles — сеанс с внешней дорожкой обязан отдать
// телевизору тот же subtitlePlaylist, что и со встроенной, и файлы под ним
// должны существовать ещё до первого сегмента.
func TestStartWritesExternalSubtitles(t *testing.T) {
	tmp := t.TempDir()
	srt := filepath.Join(tmp, "s01e01.srt")
	if err := os.WriteFile(srt, []byte(externalSRT), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		TmpDir: tmp,
		// Настоящий ffmpeg тут не нужен: проверяется то, что делает сам
		// менеджер вокруг запуска. Процесс должен просто существовать и выйти.
		FFmpeg:   "true",
		RawURL:   func(int) string { return "http://127.0.0.1:8000/raw/0" },
		NowMilli: func() int64 { return 1786870000000 },
	}
	t.Cleanup(m.Shutdown)

	snap, err := m.Start(StartOptions{
		Index: 0,
		Meta: &media.Result{
			Index: 0, Name: "s01e01.mkv", Duration: 1200,
			Video: &media.VideoInfo{Index: 0, Codec: "h264"},
			Audio: make([]media.AudioTrack, 0),
			Subtitles: []media.SubtitleTrack{
				{Index: -1, Code: "rus", Label: "Russian", Codec: "srt", SourcePath: srt},
			},
		},
		SubPref: "rus",
		Start:   0,
	})
	if err != nil {
		t.Fatal(err)
	}

	if snap.Sub != "rus" {
		t.Errorf("sub = %q", snap.Sub)
	}
	if snap.SubtitlePlaylist == nil || *snap.SubtitlePlaylist != "/hls/"+snap.ID+"/"+subs.PlaylistName {
		t.Fatalf("subtitlePlaylist = %v", snap.SubtitlePlaylist)
	}

	dir := filepath.Join(tmp, "tms-hls-"+snap.ID)
	if _, err := os.Stat(filepath.Join(dir, subs.PlaylistName)); err != nil {
		t.Errorf("плейлиста нет: %v", err)
	}
}

// TestStartSurvivesBrokenExternalSubtitles — пропавший файл субтитров гасит
// дорожку и только её: серия обязана начаться.
func TestStartSurvivesBrokenExternalSubtitles(t *testing.T) {
	tmp := t.TempDir()
	m := &Manager{
		TmpDir:   tmp,
		FFmpeg:   "true",
		RawURL:   func(int) string { return "http://127.0.0.1:8000/raw/0" },
		NowMilli: func() int64 { return 1786870000000 },
	}
	t.Cleanup(m.Shutdown)

	snap, err := m.Start(StartOptions{
		Index: 0,
		Meta: &media.Result{
			Index: 0, Name: "s01e01.mkv", Duration: 1200,
			Video: &media.VideoInfo{Index: 0, Codec: "h264"},
			Audio: make([]media.AudioTrack, 0),
			Subtitles: []media.SubtitleTrack{
				{Index: -1, Code: "rus", Label: "Russian", Codec: "srt",
					SourcePath: filepath.Join(tmp, "нет-такого.srt")},
			},
		},
		SubPref: "rus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Sub != "off" || snap.SubtitlePlaylist != nil {
		t.Errorf("дорожка не погасла: sub=%q playlist=%v", snap.Sub, snap.SubtitlePlaylist)
	}
}
