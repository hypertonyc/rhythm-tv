//go:build !unix

package metrics

import "io/fs"

// Без statfs место не измерить — дашборд покажет прочерк вместо диска.
// Файл нужен, чтобы пакет собирался везде; сервер живёт под Linux, правится
// под macOS, и обе ветки — unix.

func diskUsage(string) (total, free, used int64, ok bool) { return 0, 0, 0, false }

func fileUsage(info fs.FileInfo) int64 { return info.Size() }
