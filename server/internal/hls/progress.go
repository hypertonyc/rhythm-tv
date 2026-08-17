package hls

import (
	"strconv"
	"strings"
	"sync"
)

// progressWriter разбирает вывод `ffmpeg -progress pipe:1`.
//
// Формат простой и машинный: блоки строк `ключ=значение`, каждый блок
// заканчивается `progress=continue` (или `progress=end` на выходе). Значения
// в блоке относятся к одному моменту времени, поэтому наружу они отдаются
// только целым блоком — иначе можно было бы показать out_time из нового блока
// вместе со speed из старого.
//
// Из десятка ключей нужны два:
//
//   - out_time — сколько секунд ВЫХОДНОГО материала уже произведено. Это и есть
//     настоящий прогресс: телевизор ждёт два стартовых сегмента, то есть
//     8 секунд видео, и out_time/8 — честная доля, а не выдумка.
//   - speed — во сколько раз быстрее реального времени идёт работа. По нему
//     считается остаток: (нужно − сделано) / speed.
//
// Берётся именно `out_time`, а не `out_time_ms`: последний, вопреки имени,
// содержит МИКРОсекунды — давняя ошибка ffmpeg, которую не чинят ради обратной
// совместимости. `out_time_us` появился позже и есть не везде. Строка
// `00:00:02.048000` неоднозначной не бывает ни в одной версии.
//
// В отличие от ringWriter, у этого писателя лок есть и он обязателен: пишет
// в него копировщик exec, а читают обработчики HTTP, и происходит это
// одновременно.
type progressWriter struct {
	mu      sync.Mutex
	tail    string // недописанная строка между вызовами Write
	block   progressBlock
	current progressBlock // последний ЗАВЕРШЁННЫЙ блок
	seen    bool
}

type progressBlock struct {
	OutTimeMs int64
	Speed     float64
	HasTime   bool
	HasSpeed  bool
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.tail += string(p)
	for {
		nl := strings.IndexByte(w.tail, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimRight(w.tail[:nl], "\r")
		w.tail = w.tail[nl+1:]
		w.consumeLine(line)
	}
	// Строка без перевода в конце — законный случай (ffmpeg пишет блоками,
	// а Write режется по границам буфера), но расти бесконечно она не должна:
	// мусор на stdout вместо -progress не обязан съесть память.
	if len(w.tail) > 4096 {
		w.tail = ""
	}
	return len(p), nil
}

func (w *progressWriter) consumeLine(line string) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return
	}
	key := strings.TrimSpace(line[:eq])
	value := strings.TrimSpace(line[eq+1:])

	switch key {
	case "out_time":
		if ms, ok := parseFFmpegTime(value); ok {
			w.block.OutTimeMs = ms
			w.block.HasTime = true
		}
	case "speed":
		if sp, ok := parseFFmpegSpeed(value); ok {
			w.block.Speed = sp
			w.block.HasSpeed = true
		}
	case "progress":
		// Блок закрылся — только теперь он становится видимым снаружи.
		w.current = w.block
		w.seen = true
		w.block = progressBlock{}
	}
}

// snapshot отдаёт последний завершённый блок. seen=false означает, что ffmpeg
// не отчитался ещё ни разу: он либо не стартовал, либо сидит на чтении входа
// (для нас — ждёт данных из роя). Это РАЗНЫЕ вещи с «сделано 0 секунд»,
// и различать их обязан вызывающий.
func (w *progressWriter) snapshot() (progressBlock, bool) {
	if w == nil {
		return progressBlock{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current, w.seen
}

// parseFFmpegTime разбирает `HH:MM:SS.mmmmmm` в миллисекунды.
//
// До первого кадра ffmpeg пишет туда `N/A`, а на output-side -ss успевает
// выдать отрицательное время — и то и другое не прогресс, а «пока ничего».
func parseFFmpegTime(v string) (int64, bool) {
	if v == "" || v == "N/A" {
		return 0, false
	}
	neg := strings.HasPrefix(v, "-")
	v = strings.TrimPrefix(v, "-")

	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	ms := int64((float64(h)*3600+float64(m)*60+sec)*1000 + 0.5)
	if neg {
		return 0, true
	}
	return ms, true
}

// parseFFmpegSpeed разбирает `2.01x`. `N/A` бывает в первых блоках,
// пока средняя скорость ещё не посчитана.
func parseFFmpegSpeed(v string) (float64, bool) {
	v = strings.TrimSuffix(strings.TrimSpace(v), "x")
	if v == "" || v == "N/A" {
		return 0, false
	}
	sp, err := strconv.ParseFloat(v, 64)
	if err != nil || sp <= 0 {
		return 0, false
	}
	return sp, true
}
