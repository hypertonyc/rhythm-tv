//go:build unix

package mediasource

import (
	"os"
	"syscall"
)

// allocatedBytes — сколько места файл занимает на диске на самом деле.
//
// Отсутствующий файл — это ноль занятого, а не ошибка: файл, помеченный
// скачанным, но не существующий, — такой же фантом, как разреженный,
// и лечится тем же способом.
func allocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Файловая система без stat(2): дырок не видно, считаем файл целым,
		// чтобы не гонять перепроверку на ровном месте.
		return info.Size(), nil
	}
	// Blocks в stat(2) всегда в единицах 512 байт — независимо от того,
	// какой блок у самой файловой системы.
	return int64(st.Blocks) * 512, nil
}
