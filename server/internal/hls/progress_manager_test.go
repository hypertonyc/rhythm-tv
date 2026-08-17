package hls

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avdav/torrent-media/server/internal/media"
)

// TestProgressReachesManager — сквозная проверка проводки, а не разбора:
// stdout процесса → progressWriter → Manager.Progress. Разбор покрыт отдельно,
// а вот эта цепочка рвётся молча — достаточно кому-нибудь занять cmd.Stdout
// под что-то своё, и прогресс на экране телевизора навсегда станет
// «не измерено», ничего при этом не сломав.
func TestProgressReachesManager(t *testing.T) {
	tmp := t.TempDir()
	shim := filepath.Join(tmp, "ffmpeg-shim")
	script := "#!/bin/sh\n" +
		"printf 'out_time=00:00:06.000000\\nspeed= 112x\\nprogress=continue\\n'\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		TmpDir: tmp,
		FFmpeg: shim,
		// Копирование включено, чтобы конвейер описывал НАСТОЯЩЕЕ решение:
		// исходник ниже проходит обе проверки, и «copy» здесь — не декорация,
		// а то же самое, что уйдёт в аргументы ffmpeg.
		AllowCopy: true,
		RawURL:    func(int) string { return "http://127.0.0.1:8000/raw/0" },
		NowMilli:  func() int64 { return 1786870000000 },
	}
	t.Cleanup(m.Shutdown)

	snap, err := m.Start(StartOptions{
		Index: 0,
		Meta: &media.Result{
			Index: 0, Name: "s01e01.mkv", Duration: 1200,
			Video: &media.VideoInfo{Index: 0, Codec: "h264", Profile: "High", Level: 40,
				PixFmt: "yuv420p", Width: 1920, Height: 1080},
			Audio:     []media.AudioTrack{{Index: 1, Code: "eng", Codec: "aac", Profile: "HE-AAC", Channels: 6, SampleRate: 48000}},
			Subtitles: make([]media.SubtitleTrack, 0),
		},
		AudioPref: "eng",
		SubPref:   "off",
		Start:     0,
	})
	if err != nil {
		t.Fatal(err)
	}

	p := waitForEncoded(t, m, snap.ID)

	if p.EncodedMs == nil || *p.EncodedMs != 6000 {
		t.Fatalf("encodedMs = %v, ожидалось 6000", p.EncodedMs)
	}
	if p.Speed == nil || *p.Speed != 112 {
		t.Fatalf("speed = %v, ожидалось 112", p.Speed)
	}
	// Цель старта уезжает наружу, а не зашита в клиента: два сегмента по четыре
	// секунды. 6000 из 8000 — то есть остаток есть и он считается.
	if p.StartupTargetMs != 8000 || p.StartupSegments != 2 {
		t.Fatalf("цель старта: %d мс / %d сегментов", p.StartupTargetMs, p.StartupSegments)
	}
	// (8000 − 6000) / 112 — остаток, делённый на скорость.
	wantEta := int64(float64(p.StartupTargetMs-*p.EncodedMs) / *p.Speed)
	if p.EtaMs == nil || *p.EtaMs != wantEta {
		t.Fatalf("etaMs = %v, ожидалось %d", p.EtaMs, wantEta)
	}
	// Конвейер приезжает тем же ответом: телефону нужно и то и другое,
	// а лишний запрос на этой стороне ничего не стоит только на вид.
	if p.Pipeline.Video.Mode != "copy" || p.Pipeline.Audio == nil || p.Pipeline.Audio.Mode != "copy" {
		t.Fatalf("конвейер: %+v", p.Pipeline)
	}
}

// TestProgressUnmeasuredBeforeFFmpegSpeaks — молчащий ffmpeg (для нас это
// «ждём данных из роя») обязан отличаться от «сделано ноль»: на экране это
// разные надписи, и подменять одно другим значит врать.
func TestProgressUnmeasuredBeforeFFmpegSpeaks(t *testing.T) {
	tmp := t.TempDir()
	m := &Manager{
		TmpDir:   tmp,
		FFmpeg:   "true", // ничего не пишет в stdout вовсе
		RawURL:   func(int) string { return "http://127.0.0.1:8000/raw/0" },
		NowMilli: func() int64 { return 1786870000000 },
	}
	t.Cleanup(m.Shutdown)

	snap, err := m.Start(StartOptions{
		Index: 0,
		Meta: &media.Result{
			Index: 0, Name: "s01e01.mkv", Duration: 1200,
			Video:     &media.VideoInfo{Index: 0, Codec: "h264"},
			Audio:     make([]media.AudioTrack, 0),
			Subtitles: make([]media.SubtitleTrack, 0),
		},
		SubPref: "off",
	})
	if err != nil {
		t.Fatal(err)
	}

	p, ok := m.Progress(snap.ID)
	if !ok {
		t.Fatal("сеанс есть, а прогресса по нему нет")
	}
	if p.EncodedMs != nil || p.Speed != nil || p.EtaMs != nil {
		t.Fatalf("ffmpeg молчит, а измерения появились: %v %v %v", p.EncodedMs, p.Speed, p.EtaMs)
	}
	if p.Pipeline.Video.From == "" {
		t.Error("конвейер известен до всякого ffmpeg и обязан быть заполнен")
	}
}

func TestProgressUnknownSession(t *testing.T) {
	m := &Manager{TmpDir: t.TempDir(), FFmpeg: "true"}
	t.Cleanup(m.Shutdown)
	if _, ok := m.Progress("нет-такого"); ok {
		t.Error("несуществующий сеанс обязан отвечать «не найден»")
	}
	// Пустой id означает активный сеанс, а его нет.
	if _, ok := m.Progress(""); ok {
		t.Error("активного сеанса нет — ожидался отказ")
	}
}

func waitForEncoded(t *testing.T, m *Manager, id string) Progress {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, ok := m.Progress(id)
		if ok && p.EncodedMs != nil {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("прогресс не доехал за 5 с: %+v", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
