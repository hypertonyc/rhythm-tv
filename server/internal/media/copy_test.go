package media

import "testing"

// safeVideo — исходник, который уже ровно в целевом формате и потому копируется.
func safeVideo() *VideoInfo {
	return &VideoInfo{
		Codec: "h264", Width: 1920, Height: 1080,
		PixFmt: "yuv420p", Profile: "High", Level: 40, FieldOrder: "progressive",
	}
}

func TestCanCopyVideo(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*VideoInfo)
		start float64
		allow bool
		want  bool
	}{
		{name: "эталонный h264 high", allow: true, want: true},
		{name: "выключено через HLS_ALLOW_COPY=0", allow: false, want: false},

		// Перемотка всегда форсирует перекодирование: output-side seek режет
		// поток не по ключевому кадру.
		{name: "перемотка запрещает копирование", start: 30, allow: true, want: false},
		{name: "start в пределах порога 0.05", start: 0.05, allow: true, want: true},
		{name: "start чуть выше порога", start: 0.051, allow: true, want: false},

		{name: "не h264", mut: func(v *VideoInfo) { v.Codec = "hevc" }, allow: true, want: false},
		{name: "10 бит", mut: func(v *VideoInfo) { v.PixFmt = "yuv420p10le" }, allow: true, want: false},
		{name: "профиль вне списка", mut: func(v *VideoInfo) { v.Profile = "High 10" }, allow: true, want: false},
		{name: "профиль в другом регистре", mut: func(v *VideoInfo) { v.Profile = "MAIN" }, allow: true, want: true},
		{name: "level 41 — граница включительно", mut: func(v *VideoInfo) { v.Level = 41 }, allow: true, want: true},
		{name: "level 42", mut: func(v *VideoInfo) { v.Level = 42 }, allow: true, want: false},
		{name: "level 0 считается отсутствующим", mut: func(v *VideoInfo) { v.Level = 0 }, allow: true, want: false},

		// Унаследованная дырка: ffprobe отдаёт -99 для неизвестного уровня,
		// и оба условия его пропускают. Тест фиксирует баг Node-сервера,
		// а не одобряет его — контракт заморожен.
		{name: "level -99 проходит (баг Node, воспроизведён намеренно)",
			mut: func(v *VideoInfo) { v.Level = -99 }, allow: true, want: true},

		{name: "4K", mut: func(v *VideoInfo) { v.Width = 3840; v.Height = 2160 }, allow: true, want: false},
		{name: "1088 — граница включительно", mut: func(v *VideoInfo) { v.Height = 1088 }, allow: true, want: true},
		{name: "чересстрочное", mut: func(v *VideoInfo) { v.FieldOrder = "tt" }, allow: true, want: false},
		{name: "пустой fieldOrder разрешён", mut: func(v *VideoInfo) { v.FieldOrder = "" }, allow: true, want: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := safeVideo()
			if c.mut != nil {
				c.mut(v)
			}
			if got := CanCopyVideo(v, c.start, c.allow); got != c.want {
				t.Errorf("CanCopyVideo = %v, ожидалось %v", got, c.want)
			}
		})
	}

	if CanCopyVideo(nil, 0, true) {
		t.Error("CanCopyVideo(nil) обязан быть false")
	}
}

func TestCanCopyAudio(t *testing.T) {
	safe := func() *AudioTrack {
		return &AudioTrack{Codec: "aac", Profile: "LC", Channels: 2, SampleRate: 48000}
	}
	cases := []struct {
		name  string
		mut   func(*AudioTrack)
		start float64
		allow bool
		want  bool
	}{
		{name: "эталонный AAC-LC стерео", allow: true, want: true},
		{name: "выключено", allow: false, want: false},
		{name: "перемотка", start: 30, allow: true, want: false},
		{name: "не aac", mut: func(a *AudioTrack) { a.Codec = "ac3" }, allow: true, want: false},
		{name: "профиль в нижнем регистре", mut: func(a *AudioTrack) { a.Profile = "lc" }, allow: true, want: true},
		{name: "моно разрешено", mut: func(a *AudioTrack) { a.Channels = 1 }, allow: true, want: true},
		{name: "нет каналов", mut: func(a *AudioTrack) { a.Channels = 0 }, allow: true, want: false},
		{name: "44.1 кГц", mut: func(a *AudioTrack) { a.SampleRate = 44100 }, allow: true, want: true},
		{name: "32 кГц", mut: func(a *AudioTrack) { a.SampleRate = 32000 }, allow: true, want: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := safe()
			if c.mut != nil {
				c.mut(a)
			}
			if got := CanCopyAudio(a, c.start, c.allow); got != c.want {
				t.Errorf("CanCopyAudio = %v, ожидалось %v", got, c.want)
			}
		})
	}

	if CanCopyAudio(nil, 0, true) {
		t.Error("CanCopyAudio(nil) обязан быть false")
	}
}

// TestCanCopyAudioWiderThanNode держит осознанное расхождение с эталоном.
//
// В Node (и у нас до 17.08.2026) копировался только AAC-LC до двух каналов —
// «ровно то, во что мы иначе перекодировали бы сами». Из-за профиля и числа
// каналов перекодировался звук каждой серии «Друзей»: английская дорожка там
// HE-AAC 5.1. Проверено на телевизоре — играет; разбор рисков и того, что
// именно проверено, в комментарии к CanCopyAudio.
//
// Эти случаи НЕЛЬЗЯ переносить в testdata/args_scenarios.json: там эталон
// ответит transcode, и golden-тест упадёт. Расхождение живёт здесь.
func TestCanCopyAudioWiderThanNode(t *testing.T) {
	// Дорожка из «Друзей» s03e19, eng — ровно та, на которой это проверялось.
	heaac51 := func(mut func(*AudioTrack)) *AudioTrack {
		a := &AudioTrack{Codec: "aac", Profile: "HE-AAC", Channels: 6, SampleRate: 48000}
		if mut != nil {
			mut(a)
		}
		return a
	}

	if !CanCopyAudio(heaac51(nil), 0, true) {
		t.Error("HE-AAC 5.1 обязан копироваться: Node перекодировал бы, мы — нет")
	}

	// А это расширение НЕ трогает — иначе оно перестало бы быть про AAC.
	if CanCopyAudio(heaac51(func(a *AudioTrack) { a.Codec = "ac3" }), 0, true) {
		t.Error("AC-3 остаётся вне whitelist'а")
	}
	if CanCopyAudio(heaac51(func(a *AudioTrack) { a.SampleRate = 32000 }), 0, true) {
		t.Error("частота вне 44.1/48 кГц остаётся вне whitelist'а")
	}
	if CanCopyAudio(heaac51(func(a *AudioTrack) { a.Channels = 0 }), 0, true) {
		t.Error("дорожка без каналов не разобрана ffprobe — копировать нечего")
	}
	if CanCopyAudio(heaac51(nil), 30, true) {
		t.Error("перемотка гасит копирование звука независимо от профиля")
	}
	if CanCopyAudio(heaac51(nil), 0, false) {
		t.Error("HLS_ALLOW_COPY=0 сильнее расширенного whitelist'а")
	}
}
