//go:build !unix

package mediasource

import "os"

// allocatedBytes без stat(2) отличить дырку от записанных нулей не может,
// поэтому отвечает размером файла — то есть проверка на фантомы здесь
// всегда молчит. Сервер собирается и работает только под Linux (Dockerfile)
// и разрабатывается под macOS; ветка существует, чтобы пакет компилировался
// везде, а не чтобы что-то ловить.
func allocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}
