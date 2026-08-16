package httpapi

import "testing"

// TestParseRange фиксирует разбор Range со всеми странностями оригинала.
//
// Эти случаи и есть причина, по которой заголовок разбирается вручную,
// а не через http.ParseRange: половина строк ниже стандартной реализацией
// трактуется иначе, а через /raw ходит ffmpeg, чьё поведение менять нельзя.
func TestParseRange(t *testing.T) {
	const size = 1000

	cases := []struct {
		name             string
		header           string
		wantOK           bool
		wantStart, wantE int64
	}{
		{name: "обычный диапазон", header: "bytes=0-99", wantOK: true, wantStart: 0, wantE: 99},
		{name: "открытый справа", header: "bytes=500-", wantOK: true, wantStart: 500, wantE: 999},
		{name: "хвост файла", header: "bytes=-500", wantOK: true, wantStart: 500, wantE: 999},
		{name: "хвост длиннее файла обрезается до начала",
			header: "bytes=-5000", wantOK: true, wantStart: 0, wantE: 999},
		{name: "конец за пределами файла подрезается",
			header: "bytes=900-99999", wantOK: true, wantStart: 900, wantE: 999},
		{name: "один байт", header: "bytes=0-0", wantOK: true, wantStart: 0, wantE: 0},

		// Обе части пустые — это НЕ форма «хвост», потому что условие в оригинале
		// требует непустую вторую часть. Получается весь файл, и притом с 206.
		// http.ParseRange такую строку отвергает.
		{name: "bytes=- отдаёт весь файл", header: "bytes=-", wantOK: true, wantStart: 0, wantE: 999},

		// Нулевой хвост: start = size, end = size-1, то есть end < start.
		{name: "bytes=-0 невалиден", header: "bytes=-0", wantOK: false},

		{name: "начало за пределами файла", header: "bytes=1000-1005", wantOK: false},
		{name: "конец раньше начала", header: "bytes=500-200", wantOK: false},
		{name: "множественный диапазон не поддерживается", header: "bytes=0-1,3-4", wantOK: false},
		{name: "другая единица измерения", header: "items=0-1", wantOK: false},
		{name: "регистр важен", header: "Bytes=0-1", wantOK: false},
		{name: "пробелы не допускаются", header: "bytes = 0-1", wantOK: false},
		{name: "мусор", header: "bytes=abc-def", wantOK: false},
		{name: "пустая строка", header: "", wantOK: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, ok := parseRange(c.header, size)
			if ok != c.wantOK {
				t.Fatalf("parseRange(%q) ok = %v, ожидалось %v", c.header, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if start != c.wantStart || end != c.wantE {
				t.Errorf("parseRange(%q) = %d-%d, ожидалось %d-%d",
					c.header, start, end, c.wantStart, c.wantE)
			}
		})
	}
}

// TestRawContentType — таблица зашита в код, чтобы Content-Type не зависел
// от системного mime.types (на маке он один, в образе другой).
func TestRawContentType(t *testing.T) {
	cases := map[string]string{
		"S01E01 - Pilot.mp4": "video/mp4",
		"a.mkv":              "video/x-matroska",
		"a.MKV":              "video/x-matroska",
		"a.m4v":              "video/x-m4v",
		"a.webm":             "video/webm",
		"notes.txt":          "application/octet-stream",
		"noext":              "application/octet-stream",
	}
	for name, want := range cases {
		if got := rawContentType(name); got != want {
			t.Errorf("rawContentType(%q) = %q, ожидалось %q", name, got, want)
		}
	}
}
