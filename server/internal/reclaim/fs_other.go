//go:build !unix

package reclaim

import (
	"errors"
	"os"
	"time"
)

// Без statfs свободное место не узнать, поэтому чистка здесь не работает
// вовсе: ошибка из freeBytes останавливает проход, ничего не удаляя.
// Сервер собирается и работает только под Linux (Dockerfile) и правится
// под macOS — обе ветки unix. Этот файл существует, чтобы пакет
// компилировался везде, как и mediasource/phantom_other.go.
func freeBytes(dir string) (int64, error) {
	return 0, errors.New("свободное место на этой системе не измеряется")
}

func fileUsage(path string) (int64, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return info.Size(), info.ModTime(), nil
}
