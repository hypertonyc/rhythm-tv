package hls

import (
	"testing"

	"github.com/avdav/torrent-media/server/internal/media"
)

func TestDescribePipelineTranscode(t *testing.T) {
	v := &media.VideoInfo{Codec: "hevc", Profile: "Main 10", Level: 120, PixFmt: "yuv420p10le", Width: 3840, Height: 2160}
	a := &media.AudioTrack{Codec: "ac3", Channels: 6, SampleRate: 48000}

	p := describePipeline(v, a, false, false)

	if p.Video.Mode != "transcode" || p.Audio.Mode != "transcode" {
		t.Fatalf("режимы: video=%s audio=%s", p.Video.Mode, p.Audio.Mode)
	}
	if want := "hevc · Main 10@12.0 · yuv420p10le · 3840x2160"; p.Video.From != want {
		t.Errorf("video.from = %q, ожидалось %q", p.Video.From, want)
	}
	if want := "ac3 · 5.1 · 48.0 kHz"; p.Audio.From != want {
		t.Errorf("audio.from = %q, ожидалось %q", p.Audio.From, want)
	}
	// Цель — ровно то, что стоит в videoArgs/audioArgs. Разрешение не меняется:
	// фильтра масштабирования у нас нет вовсе, и обещать «в 1080p» было бы враньём.
	if want := "h264 · High@4.0 · yuv420p · 3840x2160 · libx264 crf 20"; p.Video.To != want {
		t.Errorf("video.to = %q, ожидалось %q", p.Video.To, want)
	}
	if want := "aac · LC · stereo · 48.0 kHz · 160k"; p.Audio.To != want {
		t.Errorf("audio.to = %q, ожидалось %q", p.Audio.To, want)
	}
}

// TestDescribePipelineCopyKeepsSource — при копировании выход равен входу,
// и показать это надо так же явно, как перекодирование: «copy» без второй
// половины оставляло бы вопрос «а на выходе-то что».
func TestDescribePipelineCopyKeepsSource(t *testing.T) {
	// Ровно та дорожка, ради которой копирование звука расширили: HE-AAC 5.1
	// из «Друзей» (см. media.CanCopyAudio).
	v := &media.VideoInfo{Codec: "h264", Profile: "High", Level: 40, PixFmt: "yuv420p", Width: 1920, Height: 1080}
	a := &media.AudioTrack{Codec: "aac", Profile: "HE-AAC", Channels: 6, SampleRate: 48000}

	p := describePipeline(v, a, true, true)

	if p.Video.From != p.Video.To || p.Audio.From != p.Audio.To {
		t.Fatalf("copy изменил дорожку: %q → %q, %q → %q",
			p.Video.From, p.Video.To, p.Audio.From, p.Audio.To)
	}
	if want := "h264 · High@4.0 · yuv420p · 1920x1080"; p.Video.From != want {
		t.Errorf("video = %q, ожидалось %q", p.Video.From, want)
	}
	if want := "aac · HE-AAC · 5.1 · 48.0 kHz"; p.Audio.From != want {
		t.Errorf("audio = %q, ожидалось %q", p.Audio.From, want)
	}
}

// TestDescribePipelineMissingPieces — у рипов сплошь и рядом нет то профиля,
// то частоты, а level -99 означает «ffprobe не разобрал» (см. CanCopyVideo).
// Показывать «@-9.-9» человеку незачем, как и пустые куски между точками.
func TestDescribePipelineMissingPieces(t *testing.T) {
	v := &media.VideoInfo{Codec: "h264", Level: -99, Width: 1280, Height: 720}
	p := describePipeline(v, nil, true, false)

	if want := "h264 · 1280x720"; p.Video.From != want {
		t.Errorf("video = %q, ожидалось %q", p.Video.From, want)
	}
	if p.Audio != nil {
		t.Errorf("дорожки звука нет — ожидался null, получено %+v", p.Audio)
	}
}

// TestDescribePipelineNoVideo — Start до сюда с nil не доходит (там
// ErrNoVideoStream), но описание обязано быть тотальным: оно уезжает в JSON,
// и паника в обработчике HTTP тут стоила бы дороже строки-заглушки.
func TestDescribePipelineNoVideo(t *testing.T) {
	p := describePipeline(nil, nil, false, false)
	if p.Video.From != "no video" {
		t.Errorf("video.from = %q", p.Video.From)
	}
}

func TestChannelsAndSampleRate(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, ""}, {1, "mono"}, {2, "stereo"}, {6, "5.1"}, {8, "7.1"}, {3, "3ch"},
	}
	for _, c := range cases {
		if got := channels(c.n); got != c.want {
			t.Errorf("channels(%d) = %q, ожидалось %q", c.n, got, c.want)
		}
	}
	if got := sampleRate(44100); got != "44.1 kHz" {
		t.Errorf("sampleRate(44100) = %q", got)
	}
	if got := sampleRate(0); got != "" {
		t.Errorf("sampleRate(0) = %q, ожидалась пустая строка", got)
	}
}
