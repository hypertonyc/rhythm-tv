package media

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// KeyframeFinder отвечает на один вопрос: где ближайший ключевой кадр не позже
// заданной секунды. Нужен он ровно для одного — чтобы перемотка шла
// копированием, а не через libx264.
//
// Копируемый поток нельзя начать с середины GOP, и ffmpeg это знает: при
// output-side -ss он отбрасывает всё до ПЕРВОГО КЛЮЧЕВОГО кадра ПОСЛЕ start,
// а звук режет точно по запрошенной секунде. Сеанс поэтому начинается
// со звука без картинки, и дырка равна остатку GOP. Измерено 18.08.2026
// на s03e23 «Друзей» (ключевые кадры 186.186 и 196.196): -ss 187 дал первый
// видеокадр на 10.596 при звуке с 1.438 — девять секунд чёрного экрана
// с диалогом. Как это переживёт AVPlay на Tizen 2.3, не проверено и проверять
// незачем: достаточно попросить точку, на которой ключевой кадр уже есть,
// и тогда поток выходит ровно таким же, как при старте с нуля, — а тот
// работает с первого дня.
//
// Отдельный тип, а не метод Prober, потому что кэшировать тут нечего: ответ
// зависит от секунды, а не от файла, и второй раз в ту же точку не перематывают.
type KeyframeFinder struct {
	// RawURL отдаёт http://127.0.0.1:<PORT>/raw/<index> — тот же путь, которым
	// читают ffprobe и ffmpeg.
	RawURL func(index int) string
	// Binary — имя ffprobe; подменяется в тестах.
	Binary string
	// Timeout невелик намеренно: не нашли ключевой кадр быстро — перекодируем,
	// как перекодировали раньше. Ждать тут дольше, чем стоит сам сеанс,
	// значит менять быструю перемотку на медленную.
	Timeout time.Duration
}

// keyframeWindow — сколько секунд перед точкой просматривается.
//
// Запаса хватает с большим избытком: ffprobe исполняет -read_intervals через
// seek, а seek у него идёт к ключевому кадру НЕ ПОЗЖЕ начала интервала,
// поэтому первая же выданная строка обычно и есть искомая. Окно нужно для
// файлов с редкими ключевыми кадрами: на «Друзьях» средний GOP 5.1 с,
// максимальный 10.0 с.
const keyframeWindow = 20.0

// Before возвращает время последнего ключевого кадра не позже start.
//
// Второе значение — «нашли». Не нашли (нет пиров, ffprobe не понял файл,
// вышел таймаут) — это не ошибка сеанса, а разрешение перекодировать:
// вызывающий просто останется при прежнем поведении.
func (f *KeyframeFinder) Before(ctx context.Context, index, videoIndex int, start float64) (float64, bool) {
	if f == nil || f.RawURL == nil || start <= 0 {
		return 0, false
	}
	binary := f.Binary
	if binary == "" {
		binary = "ffprobe"
	}
	from := start - keyframeWindow
	if from < 0 {
		from = 0
	}

	// Читаются ПАКЕТЫ, а не кадры: флаг K лежит в пакете, и декодировать
	// ради него нечего. По времени на проде это одинаково (~165 мс на файл
	// из хранилища), но пакеты не зависят от того, сможет ли декодер
	// разобрать поток.
	out, err := runCapture(ctx, binary, []string{
		"-v", "error",
		"-select_streams", strconv.Itoa(videoIndex),
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		"-read_intervals", seconds(from) + "%" + seconds(start),
		f.RawURL(index),
	}, f.Timeout)
	if err != nil {
		return 0, false
	}
	return lastKeyframeAtOrBefore(string(out), start)
}

// lastKeyframeAtOrBefore разбирает вывод `-show_entries packet=pts_time,flags
// -of csv=p=0`. Строки выглядят как `186.186000,K__`; у неключевого пакета
// на месте K стоит подчёркивание, а pts_time бывает и `N/A`.
//
// Берётся ПОСЛЕДНИЙ подходящий, а не первый: ffprobe отдаёт интервал целиком,
// и в нём обычно несколько ключевых кадров. Порядок не предполагается —
// сравнение честное, поэтому переупорядоченный вывод ничего не сломает.
func lastKeyframeAtOrBefore(out string, start float64) (float64, bool) {
	best, found := 0.0, false
	for _, line := range strings.Split(out, "\n") {
		comma := strings.LastIndexByte(line, ',')
		if comma < 0 || !strings.ContainsRune(line[comma+1:], 'K') {
			continue
		}
		pts, err := strconv.ParseFloat(strings.TrimSpace(line[:comma]), 64)
		if err != nil || pts > start {
			continue
		}
		if !found || pts > best {
			best, found = pts, true
		}
	}
	return best, found
}

func seconds(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}
