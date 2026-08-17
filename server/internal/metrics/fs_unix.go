//go:build unix

package metrics

import (
	"io/fs"
	"syscall"
)

// diskUsage — размер, свободное и занятое на файловой системе пути.
//
// Free считается по Bavail, а Used — по Blocks-Bfree, и это НЕ одна и та же
// арифметика: часть места ядро резервирует под root, нам оно недоступно,
// но занятым тоже не считается. Free здесь обязан совпадать с тем, по чему
// принимает решение чистка места (reclaim/fs_unix.go), иначе на экране будет
// одно число, а выселение начнётся по другому.
func diskUsage(path string) (total, free, used int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, false
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	free = int64(st.Bavail) * bs
	used = (int64(st.Blocks) - int64(st.Bfree)) * bs
	return total, free, used, true
}

// fileUsage — сколько файл ЗАНИМАЕТ, а не сколько в нём объявлено.
//
// Тот же расчёт, что в reclaim/fs_unix.go и mediasource/phantom_unix.go, и по
// той же причине: недокачанная серия разрежена, по размеру она выглядит полной,
// и «сколько занято скачанным» по размерам показывало бы терабайты там, где
// на диске сотня гигабайт. Blocks всегда в единицах 512 байт, независимо
// от блока файловой системы.
func fileUsage(info fs.FileInfo) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size()
	}
	return int64(st.Blocks) * 512
}
