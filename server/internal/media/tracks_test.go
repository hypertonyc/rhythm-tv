package media

import "testing"

func tags(kv ...string) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestNormalizeLanguage(t *testing.T) {
	cases := []struct {
		name      string
		tags      map[string]any
		ordinal   int
		kind      string
		wantCode  string
		wantLabel string
	}{
		{
			name:     "тег языка латиницей",
			tags:     tags("language", "rus"),
			kind:     "audio",
			wantCode: "rus", wantLabel: "Russian",
		},
		{
			name:     "название дорожки идёт в подпись",
			tags:     tags("language", "rus", "title", "Дубляж"),
			kind:     "audio",
			wantCode: "rus", wantLabel: "Russian — Дубляж",
		},
		{
			// Язык распознаётся и по названию, когда тега нет вовсе.
			name:     "кириллица в названии",
			tags:     tags("title", "Русская дорожка"),
			kind:     "audio",
			wantCode: "rus", wantLabel: "Russian — Русская дорожка",
		},
		{
			name:     "английский по тегу eng",
			tags:     tags("language", "eng"),
			kind:     "audio",
			wantCode: "eng", wantLabel: "English",
		},
		{
			// \b в JS и в Go одинаково ASCII-only, поэтому «русский» латинским
			// шаблоном не ловится — его ловит кириллический /рус/.
			name:     "кириллический шаблон, а не латинский",
			tags:     tags("title", "рус"),
			kind:     "audio",
			wantCode: "rus", wantLabel: "Russian — рус",
		},
		{
			name:     "неизвестный язык остаётся как есть",
			tags:     tags("language", "jpn"),
			kind:     "audio",
			wantCode: "jpn", wantLabel: "JPN",
		},
		{
			name:     "неизвестный язык с названием",
			tags:     tags("language", "jpn", "title", "Japanese dub"),
			kind:     "audio",
			wantCode: "jpn", wantLabel: "Japanese dub",
		},
		{
			name:     "und считается отсутствующим",
			tags:     tags("language", "und"),
			ordinal:  0,
			kind:     "audio",
			wantCode: "audio-1", wantLabel: "Audio 1",
		},
		{
			name:     "без тегов вовсе",
			tags:     nil,
			ordinal:  1,
			kind:     "audio",
			wantCode: "audio-2", wantLabel: "Audio 2",
		},
		{
			name:     "субтитры получают свой префикс",
			tags:     nil,
			ordinal:  0,
			kind:     "sub",
			wantCode: "sub-1", wantLabel: "Subtitles 1",
		},
		{
			// Ради этого случая CollapseWhitespace и не использует Go-шный \s:
			// с NBSP внутри подпись была бы другой, а следом — и code.
			name:     "NBSP в названии схлопывается",
			tags:     tags("language", "rus", "title", "Дубляж\u00a0(TVShows)"),
			kind:     "audio",
			wantCode: "rus", wantLabel: "Russian — Дубляж (TVShows)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, label := NormalizeLanguage(c.tags, c.ordinal, c.kind)
			if code != c.wantCode {
				t.Errorf("code = %q, ожидалось %q", code, c.wantCode)
			}
			if label != c.wantLabel {
				t.Errorf("label = %q, ожидалось %q", label, c.wantLabel)
			}
		})
	}
}

func TestTrackTitleTruncation(t *testing.T) {
	long := "Труднопроизносимое очень длинное название дорожки"
	got := TrackTitle(tags("title", long))
	if r := []rune(got); len(r) != 40 || r[39] != '…' {
		t.Errorf("TrackTitle не обрезал длинное название: %q (%d рун)", got, len(r))
	}
	if got := TrackTitle(tags("title", "  Дубляж  ")); got != "Дубляж" {
		t.Errorf("TrackTitle(%q) = %q", "  Дубляж  ", got)
	}
	if got := TrackTitle(nil); got != "" {
		t.Errorf("TrackTitle(nil) = %q, ожидалась пустая строка", got)
	}
}

func TestDisambiguate(t *testing.T) {
	// Две русские дорожки — дубляж и войсовер — обычное дело для рипов.
	// Первая обязана сохранить чистый 'rus', иначе сохранённое на телевизоре
	// предпочтение перестанет находиться.
	audio := []AudioTrack{
		{Code: "rus", Label: "Russian — Дубляж"},
		{Code: "rus", Label: "Russian — Войсовер"},
		{Code: "rus", Label: "Russian — Дубляж"},
		{Code: "eng", Label: "English"},
	}
	subs := []SubtitleTrack{
		{Code: "rus", Label: "Russian"},
		{Code: "rus", Label: "Russian"},
	}
	DisambiguateAll(audio, subs)

	wantCodes := []string{"rus", "rus-2", "rus-3", "eng"}
	wantLabels := []string{
		"Russian — Дубляж",
		"Russian — Войсовер",
		"Russian — Дубляж 2",
		"English",
	}
	for i := range audio {
		if audio[i].Code != wantCodes[i] {
			t.Errorf("audio[%d].Code = %q, ожидалось %q", i, audio[i].Code, wantCodes[i])
		}
		if audio[i].Label != wantLabels[i] {
			t.Errorf("audio[%d].Label = %q, ожидалось %q", i, audio[i].Label, wantLabels[i])
		}
	}

	// Субтитры нумеруются независимо от аудио: 'rus' у них снова свободен.
	if subs[0].Code != "rus" || subs[1].Code != "rus-2" {
		t.Errorf("субтитры: %q, %q — проходы должны быть независимыми", subs[0].Code, subs[1].Code)
	}
}

func TestChooseTrack(t *testing.T) {
	tracks := []AudioTrack{
		{Code: "eng", Label: "English"},
		{Code: "rus", Label: "Russian", Default: true},
		{Code: "tha", Label: "Thai"},
	}

	if got := ChooseAudio(tracks, "tha"); got == nil || got.Code != "tha" {
		t.Errorf("точное совпадение кода не сработало: %+v", got)
	}
	// Неизвестное предпочтение — откат на дорожку с disposition.default.
	if got := ChooseAudio(tracks, "jpn"); got == nil || got.Code != "rus" {
		t.Errorf("откат на default не сработал: %+v", got)
	}
	// Пустое предпочтение в JS ложно, поэтому поиск по коду пропускается.
	if got := ChooseAudio(tracks, ""); got == nil || got.Code != "rus" {
		t.Errorf("пустое предпочтение: %+v", got)
	}
	// Нет ни одной default — берётся первая.
	plain := []AudioTrack{{Code: "eng"}, {Code: "rus"}}
	if got := ChooseAudio(plain, "jpn"); got == nil || got.Code != "eng" {
		t.Errorf("откат на первую дорожку: %+v", got)
	}
	if got := ChooseAudio(nil, "rus"); got != nil {
		t.Errorf("пустой список обязан дать nil, получено %+v", got)
	}
}

func TestIsVideoName(t *testing.T) {
	yes := []string{
		"S01E01 - Pilot.mkv", "a.mp4", "a.MP4", "a.m4v", "a.M4V",
		"a.webm", "a.WebM", "a.MKV", "a.mKv",
	}
	for _, n := range yes {
		if !IsVideoName(n) {
			t.Errorf("IsVideoName(%q) = false", n)
		}
	}
	no := []string{
		"notes.txt", "a.mkv.txt", "a.avi", "a.mp3", "mkv", "",
		// (?i) в Go делает юникодный fold: без явных ASCII-классов
		// KELVIN SIGN (U+212A) совпал бы с 'k'.
		"a.m\u212av",
	}
	for _, n := range no {
		if IsVideoName(n) {
			t.Errorf("IsVideoName(%q) = true", n)
		}
	}
}
