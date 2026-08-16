package media

import (
	"os"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/jscompat"
)

// TestParseProbeMatchesNodeGolden — сравнение с эталоном, а не с ожиданиями автора.
//
// testdata/probe_multiaudio.json — настоящий вывод ffprobe по файлу с двумя
// русскими дорожками, английской и видео h264 High@4.0.
// testdata/probe_multiaudio.golden.json — то, что на этом же вводе печатает
// JSON.stringify из legacy/server.mjs. Код Node для эталона не переписывался
// руками: он вырезан из файла построчно (см. историю в плане порта), поэтому
// расхождение здесь означает ошибку в Go, а не в списывании.
//
// Проверяется байтовое равенство, то есть заодно: порядок полей, [] вместо null
// у пустых субтитров, null у next/prev, отсутствие .0 у целых и то,
// что sampleRate приехал числом, хотя ffprobe отдал его строкой.
func TestParseProbeMatchesNodeGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/probe_multiaudio.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/probe_multiaudio.golden.json")
	if err != nil {
		t.Fatal(err)
	}

	result, err := ParseProbe(raw, 0, "S01E01 - Fixture.mkv", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jscompat.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != string(want) {
		t.Errorf("разошлось с эталоном Node\nполучено: %s\nэталон:   %s", got, want)
	}
}

// TestParseProbeTypeCoercion фиксирует то, ради чего поля probeStream имеют тип any:
// ffprobe непоследователен, и одно и то же число приходит то числом, то строкой.
func TestParseProbeTypeCoercion(t *testing.T) {
	raw := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
	     "level":40,"pix_fmt":"yuv420p","profile":"High","field_order":"progressive"},
	    {"index":1,"codec_type":"audio","codec_name":"aac","profile":"LC",
	     "channels":2,"sample_rate":"48000","disposition":{"default":1}}
	  ],
	  "format": {"duration": "1440.500000"}
	}`)

	r, err := ParseProbe(raw, 3, "x.mkv", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Duration != 1440.5 {
		t.Errorf("duration из строки = %v", r.Duration)
	}
	if r.Audio[0].SampleRate != 48000 {
		t.Errorf("sampleRate из строки = %v", r.Audio[0].SampleRate)
	}
	if !r.Audio[0].Default {
		t.Error("disposition.default приходит числом 1, а не true")
	}
	if r.Video == nil || r.Video.Width != 1920 {
		t.Errorf("video = %+v", r.Video)
	}
}

// TestParseProbeEmptyListsAreNotNull — самая дорогая ошибка сериализации.
// Клиент читает meta.audio.length без проверки: null здесь гасит экран телевизора.
func TestParseProbeEmptyListsAreNotNull(t *testing.T) {
	raw := []byte(`{"streams":[],"format":{"duration":"0"}}`)
	r, err := ParseProbe(raw, 0, "empty.mkv", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jscompat.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"index":0,"name":"empty.mkv","duration":0,"video":null,"audio":[],"subtitles":[],"next":null,"prev":null}`
	if string(got) != want {
		t.Errorf("получено %s\nожидалось %s", got, want)
	}
}

// TestParseProbeFirstVideoStreamWins — в MKV обложка приезжает вторым
// видеопотоком, и мапить в ffmpeg надо не её.
func TestParseProbeFirstVideoStreamWins(t *testing.T) {
	raw := []byte(`{"streams":[
	  {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
	  {"index":4,"codec_type":"video","codec_name":"mjpeg","width":600,"height":900}
	]}`)
	r, err := ParseProbe(raw, 0, "x.mkv", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Video.Index != 0 || r.Video.Codec != "h264" {
		t.Errorf("выбран не первый видеопоток: %+v", r.Video)
	}
}

// TestParseProbeNextPrevArePresentNulls — ключи обязаны БЫТЬ и быть null.
// Клиент сравнивает `meta.next === null`; отсутствующий ключ даст undefined,
// сравнение не пройдёт, и автопереход сработает на последней серии.
func TestParseProbeNextPrevArePresentNulls(t *testing.T) {
	next := 7
	r, err := ParseProbe([]byte(`{"streams":[]}`), 5, "x.mkv", &next, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jscompat.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); !strings.Contains(s, `"next":7`) || !strings.Contains(s, `"prev":null`) {
		t.Errorf("next/prev сериализованы неверно: %s", s)
	}
}
