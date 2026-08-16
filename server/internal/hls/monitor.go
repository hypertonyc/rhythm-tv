package hls

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var segmentName = regexp.MustCompile(`^seg(\d+)\.ts$`)

// pollSegments пересчитывает готовые сегменты инкрементально.
//
// ffmpeg нумерует сегменты по порядку, а `-hls_flags temp_file` делает
// появление файла атомарным, поэтому готовый сегмент уже не изменится —
// достаточно посчитать его один раз. Полный обход каталога на каждом опросе
// стоил бы O(числа сегментов) вызовов stat: у 45-минутного эпизода это
// ~700 файлов каждые 500 мс.
//
// Вызывается ТОЛЬКО под Manager.mu. Оригинал держал на этих statSync весь
// event loop, так что лок здесь честнее, чем попытка схитрить.
func pollSegments(s *Session, now int64) {
	if s.nextSeq == nil {
		// Первый проход: узнаём, с какого номера ffmpeg начал нумерацию,
		// чтобы не завязываться на значение -start_number.
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return
		}
		lowest := -1
		for _, e := range entries {
			m := segmentName.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			seq, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if lowest < 0 || seq < lowest {
				lowest = seq
			}
		}
		if lowest < 0 {
			// Ни одного сегмента ещё нет — попробуем на следующем тике.
			return
		}
		s.nextSeq = &lowest
	}

	for {
		name := fmt.Sprintf("seg%05d.ts", *s.nextSeq)
		st, err := os.Stat(filepath.Join(s.dir, name))
		if err != nil {
			// Выходим на ЛЮБОЙ ошибке, не только на «нет файла»: так же
			// вёл себя catch в оригинале.
			break
		}
		s.segments++
		s.bytesOut += st.Size()
		*s.nextSeq++
		s.lastOutputAt = &now
	}
}
