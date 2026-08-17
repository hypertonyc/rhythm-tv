//go:build !linux

package metrics

import "os"

// Без /proc нагрузку машины не узнать, и врать об этом нельзя: страница
// покажет прочерк, а не плоскую линию на нуле. Сервер собирается и работает
// под Linux (Dockerfile), а правится под macOS — этот файл существует ровно
// затем, чтобы пакет компилировался и тестировался на второй из них,
// как mediasource/phantom_other.go и reclaim/fs_other.go.

func selfPID() int { return os.Getpid() }

func readHost() (Host, cpuTimes, bool) { return Host{}, cpuTimes{}, false }

func readProc(int) (Proc, procTimes, bool) { return Proc{}, procTimes{}, false }
