package hls

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/media"
)

// scenario — вход одного случая. Имена полей совпадают с json-тегами media.*,
// поэтому один и тот же файл сценариев читают и Go-тест, и Node-эталон.
type scenario struct {
	Name      string `json:"name"`
	Index     int    `json:"index"`
	Dir       string `json:"dir"`
	Start     float64
	AllowCopy bool `json:"allowCopy"`
	Meta      struct {
		Video *media.VideoInfo `json:"video"`
	} `json:"meta"`
	Audio    *media.AudioTrack    `json:"audio"`
	Subtitle *media.SubtitleTrack `json:"subtitle"`
}

type goldenEntry struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

// TestBuildArgsMatchesNodeGolden — главный тест порта.
//
// Эталон в testdata/args.golden.json получен запуском testdata/replay-args.mjs,
// который СОБРАН ИЗ СТРОК legacy/server.mjs (строки 271-314 и 486-541), а не
// переписан руками. Тест гоняет те же сценарии через media.CanCopy* и BuildArgs
// и требует полного совпадения argv.
//
// Одним ударом проверяются: порядок аргументов, позиция -ss после -i, формат
// toFixed(3), решения copy/transcode, срезание суффикса дизамбигуации в
// -var_stream_map и отсутствие a:0, когда звука нет.
//
// Пересобрать эталон (нужен Docker, node в системе не установлен):
//
//	docker run --rm -v "$PWD/testdata:/w" -w /w node:22-slim \
//	    node replay-args.mjs args_scenarios.json > testdata/args.golden.json
func TestBuildArgsMatchesNodeGolden(t *testing.T) {
	var scenarios []scenario
	readJSON(t, "testdata/args_scenarios.json", &scenarios)

	var golden []goldenEntry
	readJSON(t, "testdata/args.golden.json", &golden)

	if len(scenarios) != len(golden) {
		t.Fatalf("сценариев %d, эталонов %d — файлы разъехались", len(scenarios), len(golden))
	}

	for i, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if golden[i].Name != sc.Name {
				t.Fatalf("порядок сценариев разъехался: %q против %q", golden[i].Name, sc.Name)
			}

			got := BuildArgs(Params{
				RawURL:     "http://127.0.0.1:8000/raw/" + strconv.Itoa(sc.Index),
				Dir:        sc.Dir,
				VideoIndex: sc.Meta.Video.Index,
				Audio:      sc.Audio,
				Subtitle:   sc.Subtitle,
				Start:      sc.Start,
				CopyVideo:  media.CanCopyVideo(sc.Meta.Video, sc.Start, sc.AllowCopy),
				CopyAudio:  sc.Audio != nil && media.CanCopyAudio(sc.Audio, sc.Start, sc.AllowCopy),
			})

			if !equalArgs(got, golden[i].Args) {
				t.Errorf("argv разошлись\nполучено: %s\nэталон:   %s",
					strings.Join(got, " "), strings.Join(golden[i].Args, " "))
			}
		})
	}
}

// TestBuildArgsSeekPlacement фиксирует то, что легко «поправить» при рефакторинге:
// -ss обязан стоять ПОСЛЕ -i. Перенос его вперёд ускорил бы перемотку,
// но развалил бы тайминги встроенных субтитров.
func TestBuildArgsSeekPlacement(t *testing.T) {
	args := BuildArgs(Params{
		RawURL: "http://127.0.0.1:8000/raw/1", Dir: "/tmp/d", VideoIndex: 0, Start: 90,
	})
	iInput, iSeek := indexOf(args, "-i"), indexOf(args, "-ss")
	if iSeek < 0 {
		t.Fatal("-ss не попал в аргументы")
	}
	if iSeek < iInput {
		t.Errorf("-ss оказался перед -i: %s", strings.Join(args, " "))
	}
	if args[iSeek+1] != "90.000" {
		t.Errorf("-ss %q, ожидалось 90.000 (toFixed(3))", args[iSeek+1])
	}
}

// TestBuildArgsOutputIsLast — плейлист обязан быть последним аргументом,
// иначе ffmpeg примет его за значение предыдущего ключа.
func TestBuildArgsOutputIsLast(t *testing.T) {
	args := BuildArgs(Params{RawURL: "u", Dir: "/tmp/d", VideoIndex: 0})
	if last := args[len(args)-1]; last != "/tmp/d/index.m3u8" {
		t.Errorf("последний аргумент = %q", last)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
