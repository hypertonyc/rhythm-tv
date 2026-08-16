package subs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleSRT = "1\r\n" +
	"00:00:10,500 --> 00:00:12,000\r\n" +
	"Первая\r\n" +
	"\r\n" +
	"2\r\n" +
	"00:16:33,514 --> 00:16:35,014\r\n" +
	"Вторая строка\r\n" +
	"и её продолжение\r\n"

func TestParseCues(t *testing.T) {
	cues := parseCues(sampleSRT)
	if len(cues) != 2 {
		t.Fatalf("реплик %d, ожидалось 2: %+v", len(cues), cues)
	}
	if cues[0].start != 10.5 || cues[0].end != 12 || cues[0].text != "Первая" {
		t.Errorf("первая реплика: %+v", cues[0])
	}
	if cues[1].start != 993.514 || cues[1].text != "Вторая строка\nи её продолжение" {
		t.Errorf("вторая реплика: %+v", cues[1])
	}
}

// TestParseCuesTolerant — файл собран кем угодно, и одна кривая реплика
// не должна утаскивать за собой остальные.
func TestParseCuesTolerant(t *testing.T) {
	cues := parseCues(strings.Join([]string{
		"WEBVTT",
		"",
		"NOTE тут комментарий без времени",
		"",
		"00:00:05.000 --> сломано",
		"Это выбрасывается",
		"",
		"00:00:09.000 --> 00:00:08.000",
		"И это тоже: конец раньше начала",
		"",
		"00:00:20.000 --> 00:00:21.000 align:start position:50%",
		"А это остаётся",
		"",
		"00:00:30.000 --> 00:00:31.000",
		"",
	}, "\n"))
	if len(cues) != 1 || cues[0].text != "А это остаётся" || cues[0].start != 20 {
		t.Fatalf("осталось %d реплик: %+v", len(cues), cues)
	}
}

func TestParseTimeFormats(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"00:00:01,994", 1.994, true},
		{"00:00:01.994", 1.994, true},
		{"01:02:03.000", 3723, true},
		{" 02:03.500 ", 123.5, true},
		{"03.500", 0, false},
		{"1:2:3:4", 0, false},
		{"aa:bb.cc", 0, false},
	} {
		got, ok := parseTime(c.raw)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseTime(%q) = %v, %v; ожидалось %v, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// TestRenderVTTShiftsBySessionStart — ради этого весь пакет и написан.
//
// ffmpeg режет HLS с нуля при любом start, поэтому реплика на 16:33.514
// в сеансе, запущенном с 991.52, обязана лечь под метку 00:00:01.994.
// Без сдвига субтитры спешат ровно на start, и заметно это только
// при продолжении с середины.
func TestRenderVTTShiftsBySessionStart(t *testing.T) {
	out := renderVTT(parseCues(sampleSRT), 991.52)

	if !strings.HasPrefix(out, "WEBVTT\n") {
		t.Fatalf("нет шапки WEBVTT:\n%s", out)
	}
	// Реплика на 10.5 с кончилась задолго до места старта — её быть не должно.
	if strings.Contains(out, "Первая") {
		t.Errorf("реплика до start не выброшена:\n%s", out)
	}
	if !strings.Contains(out, "00:00:01.994 --> 00:00:03.494\nВторая строка\nи её продолжение\n") {
		t.Errorf("реплика не сдвинулась:\n%s", out)
	}
}

// TestRenderVTTClampsStraddlingCue — реплику, начавшуюся ДО места старта
// и ещё не кончившуюся, теряем только наполовину: прижимаем к нулю.
func TestRenderVTTClampsStraddlingCue(t *testing.T) {
	out := renderVTT([]cue{{start: 9, end: 15, text: "Через край"}}, 10)
	if !strings.Contains(out, "00:00:00.000 --> 00:00:05.000\nЧерез край\n") {
		t.Errorf("реплика не прижата к нулю:\n%s", out)
	}
}

func TestStamp(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{0, "00:00:00.000"},
		{1.9945, "00:00:01.995"},
		{-0.5, "00:00:00.000"},
		{3723.001, "01:02:03.001"},
	} {
		if got := stamp(c.in); got != c.want {
			t.Errorf("stamp(%v) = %s, ожидалось %s", c.in, got, c.want)
		}
	}
}

func TestWriteSession(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "episode.srt")
	if err := os.WriteFile(src, []byte(sampleSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "session")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := WriteSession(dir, src, 0, 1200); err != nil {
		t.Fatal(err)
	}

	playlist, err := os.ReadFile(filepath.Join(dir, PlaylistName))
	if err != nil {
		t.Fatal(err)
	}
	// Телевизор выбирает из плейлиста строки, оканчивающиеся на .vtt,
	// и грузит каждую ровно один раз (client/js/app.js, pollSubtitlePlaylist).
	if !strings.Contains(string(playlist), "\n"+segmentName+"\n") {
		t.Errorf("в плейлисте нет сегмента:\n%s", playlist)
	}
	if !strings.HasSuffix(string(playlist), "#EXT-X-ENDLIST\n") {
		t.Errorf("плейлист не закрыт ENDLIST:\n%s", playlist)
	}
	if !strings.Contains(string(playlist), "#EXT-X-TARGETDURATION:1200\n") {
		t.Errorf("не та TARGETDURATION:\n%s", playlist)
	}

	segment, err := os.ReadFile(filepath.Join(dir, segmentName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(segment), "00:00:10.500 --> 00:00:12.000\nПервая\n") {
		t.Errorf("не та реплика:\n%s", segment)
	}
}

// TestWriteSessionEmptyFile — пустой или неразобранный файл это ошибка,
// а не пустая дорожка: вызывающий тогда гасит субтитры и пишет в журнал,
// вместо того чтобы показывать зрителю вечное молчание.
func TestWriteSessionEmptyFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "empty.srt")
	if err := os.WriteFile(src, []byte("мусор без таймингов\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSession(tmp, src, 0, 100); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if _, err := os.Stat(filepath.Join(tmp, PlaylistName)); !os.IsNotExist(err) {
		t.Error("плейлист не должен был появиться")
	}
}

// TestWriteSessionUnknownDuration — ffprobe не всегда отдаёт длительность,
// но плейлист обязан остаться корректным.
func TestWriteSessionUnknownDuration(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "episode.srt")
	if err := os.WriteFile(src, []byte(sampleSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSession(tmp, src, 500, 0); err != nil {
		t.Fatal(err)
	}
	playlist, err := os.ReadFile(filepath.Join(tmp, PlaylistName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playlist), "#EXT-X-TARGETDURATION:1\n") {
		t.Errorf("плейлист с неизвестной длительностью:\n%s", playlist)
	}
}
