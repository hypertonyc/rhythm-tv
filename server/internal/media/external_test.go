package media

import (
	"context"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/jscompat"
)

func externalResult() *Result {
	return &Result{
		Index: 3, Name: "s01e01.mkv", Duration: 1200,
		Video: &VideoInfo{Index: 0, Codec: "h264"},
		Audio: []AudioTrack{{Index: 1, Code: "eng", Label: "English"}},
		Subtitles: []SubtitleTrack{
			{Index: 2, RelativeIndex: 0, Code: "eng", Label: "English", Codec: "subrip"},
		},
	}
}

// TestAttachExternalDisambiguates — внешний rus рядом со встроенным rus
// обязан получить свой код: ChooseTrack ищет дорожку по коду, и вторая
// с тем же кодом была бы недостижима.
func TestAttachExternalDisambiguates(t *testing.T) {
	r := externalResult()
	r.Subtitles = append(r.Subtitles, SubtitleTrack{
		Index: 3, RelativeIndex: 1, Code: "rus", Label: "Russian", Codec: "subrip",
	})

	AttachExternal(r, []SubtitleTrack{
		{Code: "rus", Label: "Russian", Codec: "srt", SourcePath: "/subs/s01e01.srt"},
	})

	if len(r.Subtitles) != 3 {
		t.Fatalf("дорожек %d: %+v", len(r.Subtitles), r.Subtitles)
	}
	ext := r.Subtitles[2]
	if ext.Code != "rus-2" || ext.Label != "Russian 2" {
		t.Errorf("внешняя дорожка: %s / %s", ext.Code, ext.Label)
	}
	// Встроенные не должны переехать: телевизор помнит их коды.
	if r.Subtitles[0].Code != "eng" || r.Subtitles[1].Code != "rus" {
		t.Errorf("встроенные дорожки поехали: %+v", r.Subtitles[:2])
	}
	if !ext.External() || ext.Index != -1 {
		t.Errorf("внешняя дорожка не помечена: %+v", ext)
	}
	if got := ChooseSubtitle(r.Subtitles, "rus-2"); got == nil || got.SourcePath != "/subs/s01e01.srt" {
		t.Errorf("по коду находится не та дорожка: %+v", got)
	}
}

// TestAttachExternalKeepsWireFormat — /api/probe сверяется с Node-эталоном
// побайтово, и путь к файлу на сервере в ответ попадать не должен.
func TestAttachExternalKeepsWireFormat(t *testing.T) {
	r := externalResult()
	AttachExternal(r, []SubtitleTrack{
		{Code: "rus", Label: "Russian", Codec: "srt", SourcePath: "/subs/s01e01.srt"},
	})

	out, err := jscompat.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "/subs/") || strings.Contains(strings.ToLower(string(out)), "sourcepath") {
		t.Errorf("путь протёк в ответ: %s", out)
	}
	want := `{"index":-1,"relativeIndex":1,"code":"rus","label":"Russian","codec":"srt","default":false}`
	if !strings.Contains(string(out), want) {
		t.Errorf("дорожка выглядит не так:\n%s\nожидалось вхождение\n%s", out, want)
	}
}

// TestAttachExternalNoop — без внешних дорожек разбор не должен меняться вовсе.
func TestAttachExternalNoop(t *testing.T) {
	r := externalResult()
	before, err := jscompat.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	AttachExternal(r, nil)
	after, err := jscompat.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("ответ изменился:\n%s\n%s", before, after)
	}
}

// TestProberAttachesExternal — внешние дорожки обязаны доехать через Prober,
// а не только через AttachExternal: ffprobe про файлы рядом с сервером
// не знает ничего, и приписывает их именно разбор.
func TestProberAttachesExternal(t *testing.T) {
	p, runs := fakeProber(t)
	req := Request{Index: 7, Name: "S01E01.mkv", External: []SubtitleTrack{
		{Code: "rus", Label: "Russian", Codec: "srt", SourcePath: "/subs/S01E01.srt"},
	}}

	result, err := p.Probe(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	last := result.Subtitles[len(result.Subtitles)-1]
	if !last.External() || last.SourcePath != "/subs/S01E01.srt" {
		t.Fatalf("внешняя дорожка не приписалась: %+v", result.Subtitles)
	}

	// Второй запрос обязан прийти из кэша вместе с ней, а не запустить ffprobe.
	again, err := p.Probe(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Subtitles) != len(result.Subtitles) {
		t.Errorf("дорожки размножились: %d против %d", len(again.Subtitles), len(result.Subtitles))
	}
	if n := runs(); n != 1 {
		t.Errorf("ffprobe запускался %d раз(а)", n)
	}
}

func TestLanguageFor(t *testing.T) {
	for _, c := range []struct {
		token, code string
		ok          bool
	}{
		{"rus", "rus", true},
		{"RU", "rus", true},
		{"Russian", "rus", true},
		{"русские", "rus", true},
		{"eng", "eng", true},
		{"ranger", "", false},
		{"s01", "", false},
		{"", "", false},
	} {
		code, _, ok := LanguageFor(c.token)
		if ok != c.ok || code != c.code {
			t.Errorf("LanguageFor(%q) = %q, %v; ожидалось %q, %v", c.token, code, ok, c.code, c.ok)
		}
	}
}
