package subs

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Имена файлов, которые кладутся в каталог сеанса.
//
// PlaylistName совпадает с тем, что делает ffmpeg для встроенной дорожки
// (index_vtt.m3u8), и это не совпадение: снимок сеанса собирает адрес
// subtitlePlaylist по одному правилу для обоих случаев, а телевизор не должен
// различать, откуда взялись субтитры.
const (
	PlaylistName = "index_vtt.m3u8"
	segmentName  = "subs0.vtt"
)

// cue — одна реплика. Времена в секундах от начала ФАЙЛА.
type cue struct {
	start, end float64
	text       string
}

// WriteSession раскладывает субтитры из файла на диске в каталог сеанса.
//
// Почему не отдать файл самому ffmpeg вторым входом: не работает. -ss у нас
// стоит ПОСЛЕ -i (output-side seek), и в этом режиме ffmpeg выбрасывает
// реплики раньше start, но оставшимся время НЕ пересчитывает — видео едет
// с нуля, а реплика 30-й секунды так и остаётся на 30-й. Проверено
// на ffmpeg 8.0; переставлять -ss нельзя, на нём держатся тайминги
// встроенных дорожек.
//
// Поэтому WebVTT собирается здесь. Заодно это дешевле (ffmpeg не открывает
// третий поток), субтитры готовы ЕЩЁ ДО первого сегмента, и они переживают
// выкатку вместе с каталогом сеанса — его подбирает следующий процесс.
//
// start — та же величина, что уходит в -ss: HLS всегда нарезается с нуля,
// поэтому метки сдвигаются на неё. Иначе при продолжении с середины
// субтитры спешат ровно на start (см. client/js/app.js, pollSubtitlePlaylist).
func WriteSession(dir, path string, start, duration float64) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cues := parseCues(decode(raw))
	if len(cues) == 0 {
		return fmt.Errorf("%s: не нашлось ни одной реплики", filepath.Base(path))
	}

	remaining := duration - start
	if remaining <= 0 {
		// Длительность бывает нулевой, если ffprobe её не отдал. Плейлист
		// всё равно обязан быть корректным, а телевизор EXTINF не читает:
		// он выбирает из плейлиста строки с .vtt и грузит их один раз.
		remaining = 1
	}

	if err := os.WriteFile(filepath.Join(dir, segmentName), []byte(renderVTT(cues, start)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, PlaylistName), []byte(renderPlaylist(remaining)), 0o644)
}

// renderPlaylist собирает плейлист из одного сегмента на всю серию.
//
// Дробить незачем: весь текст серии это десятки килобайт, телевизор скачает
// его одним запросом и больше к нему не вернётся (loadSubtitleSegment
// запоминает уже загруженные адреса).
func renderPlaylist(duration float64) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(duration)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprintf(&b, "#EXTINF:%.6f,\n", duration)
	b.WriteString(segmentName + "\n")
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// renderVTT печатает реплики, сдвинутые на start.
func renderVTT(cues []cue, start float64) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		from, to := c.start-start, c.end-start
		// Реплика, кончившаяся до места старта, не нужна вовсе; начавшуюся
		// до него прижимаем к нулю, чтобы не потерять её середину.
		if to <= 0 {
			continue
		}
		if from < 0 {
			from = 0
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n", stamp(from), stamp(to), c.text)
	}
	return b.String()
}

// stamp печатает HH:MM:SS.mmm. Часы не сокращаются: разбор на телевизоре
// различает форматы по числу двоеточий, и три части ему понятнее двух.
func stamp(t float64) string {
	ms := int64(math.Round(t * 1000))
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d",
		ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// parseCues разбирает и SRT, и WebVTT: формат блоков у них общий, различаются
// только разделитель миллисекунд (запятая против точки) и шапка.
//
// Разбор нарочно снисходительный. Файл субтитров приезжает из раздачи,
// собран кем угодно и чем угодно, и одна кривая реплика не повод потерять
// остальные семьсот.
func parseCues(text string) []cue {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var cues []cue
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(block, "\n")
		arrow := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				arrow = i
				break
			}
		}
		if arrow < 0 {
			continue
		}
		left, right, ok := strings.Cut(lines[arrow], "-->")
		if !ok {
			continue
		}
		// Справа после времени бывают настройки позиции (align:start и т.п.) —
		// берём только первое поле.
		tail := strings.Fields(right)
		if len(tail) == 0 {
			continue
		}
		from, okFrom := parseTime(left)
		to, okTo := parseTime(tail[0])
		if !okFrom || !okTo || to <= from {
			continue
		}
		body := strings.TrimSpace(strings.Join(lines[arrow+1:], "\n"))
		if body == "" {
			continue
		}
		cues = append(cues, cue{start: from, end: to, text: body})
	}
	return cues
}

// parseTime разбирает HH:MM:SS,mmm, HH:MM:SS.mmm и MM:SS.mmm.
func parseTime(raw string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(strings.ReplaceAll(raw, ",", ".")), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 {
			return 0, false
		}
		total = total*60 + v
	}
	return total, true
}
