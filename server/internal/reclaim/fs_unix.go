//go:build unix

package reclaim

import (
	"os"
	"syscall"
	"time"
)

// freeBytes — сколько на файловой системе свободно ДЛЯ НАС.
//
// Bavail, а не Bfree: часть места ядро резервирует под root (на ext4
// по умолчанию 5%), и обычному процессу оно недоступно. Считать по Bfree
// значило бы думать, что запас ещё есть, ровно тогда, когда ffmpeg уже
// получает ENOSPC посреди серии.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// fileUsage — сколько файл занимает на диске и когда его меняли в последний раз.
//
// Занятое место, а не размер: недокачанный файл разрежен, и по размеру он
// выглядит как полный. Выселение такого освободит не гигабайт, а сотню
// мегабайт, и считать надо то, что действительно вернётся.
//
// Тот же расчёт, что в mediasource/phantom_unix.go (там он ищет фантомов);
// повторён, потому что пакеты разные, а stat(2) один: Blocks всегда в единицах
// 512 байт, независимо от блока файловой системы.
func fileUsage(path string) (int64, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size(), info.ModTime(), nil
	}
	return int64(st.Blocks) * 512, info.ModTime(), nil
}
