package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/mediasource"
	"github.com/avdav/torrent-media/server/internal/subs"
)

// fakeFFprobe — подделка ffprobe: печатает один видеопоток и один звуковой,
// чего хватает ParseProbe, и считает запуски в файле.
func fakeFFprobe(t *testing.T) (binary string, runs func() int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffprobe — shell-скрипт")
	}
	dir := t.TempDir()
	payload := `{"streams":[` +
		`{"index":0,"codec_type":"video","codec_name":"h264","width":768,"height":432,` +
		`"pix_fmt":"yuv420p","profile":"High","level":41,"field_order":"progressive"},` +
		`{"index":1,"codec_type":"audio","codec_name":"aac","profile":"LC","channels":2,` +
		`"sample_rate":"48000"}],"format":{"duration":"1334.388"}}`
	out := filepath.Join(dir, "out.json")
	if err := os.WriteFile(out, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(dir, "runs")
	binary = filepath.Join(dir, "ffprobe-fake")
	script := "#!/bin/sh\necho x >> " + counter + "\ncat " + out + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, func() int {
		data, err := os.ReadFile(counter)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}
}

// oneFileSource — источник из одного файла с нужным именем. Индекс файла
// поэтому один и тот же в обоих торрентах, ровно как на проде: там под
// индексом 159 в «Друзьях» лежала s05e11, а в «Теории» — S08E01.
func oneFileSource(t *testing.T, torrent, name string) mediasource.Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := mediasource.NewFake(torrent, path)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// TestProbeFollowsActiveTorrent — 20.08.2026: торрент переключили с телефона
// с «Друзей» на «Теорию большого взрыва», и телевизор играл картинку и звук
// новой серии под именем и субтитрами старой.
//
// Причина была в кэше разбора: ключом ему был индекс файла, а индекс
// принадлежит торренту. На проде /api/files под индексом 159 отдавал
// «S08E01 - The Locomotion Interruption.mp4», а /api/probe/159 — «s05e11 -
// The One with All the Resolutions.mkv» с дорожками «Друзей» и внешними
// русскими субтитрами от них же.
//
// Проверяется здесь, а не только в media: ключ кэша собирает этот слой —
// разбор ключуется путём файла в хранилище, который знает только источник.
func TestProbeFollowsActiveTorrent(t *testing.T) {
	binary, runs := fakeFFprobe(t)

	friends := oneFileSource(t, "Друзья", "s05e11 - The One with All the Resolutions.mkv")
	tbbt := oneFileSource(t, "Big Bang Theory", "S08E01 - The Locomotion Interruption.mp4")

	// Пак русских субтитров к «Друзьям» — тот, ради которого внешние дорожки
	// и делались. Он обязан остаться при своей серии и не приклеиться к чужой.
	subsDir := filepath.Join(t.TempDir(), "frnds_sub_rus_s01-s10")
	if err := os.MkdirAll(subsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srt := filepath.Join(subsDir, "s05e11 - The One with All the Resolutions.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nпривет\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, current: friends, activeID: strings.Repeat("c", 40)}
	s := New(Deps{
		Library: lib,
		HLS:     &fakeSessions{calls: &calls},
		Prober:  &media.Prober{Binary: binary, RawURL: func(i int) string { return "http://127.0.0.1:0/raw/" + strconv.Itoa(i) }},
		Subs:    subs.New(subsDir),
	})

	name, external := probeName(t, s)
	if name != "s05e11 - The One with All the Resolutions.mkv" {
		t.Fatalf("до переключения разбор отдал %q", name)
	}
	if external != 1 {
		t.Fatalf("внешних дорожек субтитров %d, ожидалась одна: пак к «Друзьям» потерялся", external)
	}

	// Переключение торрента с телефона: /api/torrents/<id>/activate меняет
	// источник, индекс при этом остаётся тем же.
	lib.current = tbbt

	name, external = probeName(t, s)
	if name != "S08E01 - The Locomotion Interruption.mp4" {
		t.Errorf("после переключения разбор отдал %q: кэш ключуется индексом, а не файлом", name)
	}
	if external != 0 {
		t.Errorf("внешних дорожек субтитров %d, ожидалось ноль: субтитры «Друзей» приклеились к чужой серии", external)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два", got)
	}

	// Тот же файл второй раз — из кэша, без нового ffprobe.
	if _, _, err := probe(s); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два: кэш перестал держать свой же файл", got)
	}
}

func probe(s *Server) (string, int, error) {
	rec := do(s, http.MethodGet, "/api/probe/0", nil)
	var body struct {
		Name      string `json:"name"`
		Subtitles []struct {
			Index int `json:"index"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return "", 0, err
	}
	external := 0
	for _, sub := range body.Subtitles {
		// У внешней дорожки нет потока в файле, поэтому index = -1.
		if sub.Index < 0 {
			external++
		}
	}
	return body.Name, external, nil
}

func probeName(t *testing.T, s *Server) (string, int) {
	t.Helper()
	name, external, err := probe(s)
	if err != nil {
		t.Fatal(err)
	}
	return name, external
}
