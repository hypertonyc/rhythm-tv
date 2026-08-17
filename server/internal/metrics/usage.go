package metrics

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// «Сколько занимаем мы» — обход каталогов.
//
// Единственная тяжёлая операция пакета, поэтому она идёт своим ритмом (раз
// в минуту) и в своей горутине. Ошибки глотаются молча и целиком: это
// показание для человека, и половина суммы хуже честного прочерка.

// hlsPrefix — имя каталога сеанса, как его создаёт hls.Manager.
const hlsPrefix = "tms-hls-"

// dirUsage — сколько каталог занимает на диске.
func dirUsage(root string) (int64, bool) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// Файл могли удалить прямо под нами — качается и чистится
			// хранилище постоянно. Это не повод бросать весь обход.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += fileUsage(info)
		return nil
	})
	if err != nil {
		return 0, false
	}
	return total, true
}

// sessionsUsage складывает каталоги сеансов HLS во временном каталоге.
//
// Обходить TMPDIR целиком нельзя: там лежит чужое, и «мы занимаем» превратилось
// бы в «в /tmp что-то есть». Считаются только свои каталоги, по тому же имени,
// по которому их подбирает hls.AdoptOrphans.
func sessionsUsage(tmp string, walk func(string) (int64, bool)) (int64, bool) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return 0, false
	}
	var total int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), hlsPrefix) {
			continue
		}
		if n, ok := walk(filepath.Join(tmp, e.Name())); ok {
			total += n
		}
	}
	return total, true
}
