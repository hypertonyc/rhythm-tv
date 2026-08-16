package mediasource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPhantom(t *testing.T) {
	const mb = 1 << 20

	cases := []struct {
		name      string
		length    int64
		allocated int64
		want      bool
	}{
		{"целый файл", 180 * mb, 179 * mb, false},
		{"целый с запасом на метаданные ФС", 180 * mb, 181 * mb, false},
		// Ровно та авария: 184 МБ по размеру, 1.6 МБ на диске.
		{"фантом из инцидента 16.08.2026", 184968120, 1638400, true},
		{"файла нет вовсе", 180 * mb, 0, true},
		{"ровно на пороге — ещё не фантом", 100 * mb, 50 * mb, false},
		{"чуть ниже порога — уже фантом", 100 * mb, 50*mb - 1, true},
		// Мелочь не проверяется: у неё отношение занятого к размеру скачет
		// само по себе, а ловить в ней нечего.
		{"мелкий файл не проверяется", 1 * mb, 0, false},
		{"файл ровно на нижней границе проверки", 16 * mb, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPhantom(c.length, c.allocated); got != c.want {
				t.Errorf("isPhantom(%d, %d) = %v, ожидалось %v",
					c.length, c.allocated, got, c.want)
			}
		})
	}
}

func TestAllocatedBytesRealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written")
	// 2 МБ настоящих данных: важно, что не нулей — нулевой буфер некоторые
	// файловые системы умеют не записывать.
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := allocatedBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if got < int64(len(data))/2 {
		t.Errorf("записанный файл занимает %d байт при размере %d — ожидалось сопоставимо",
			got, len(data))
	}
	if isPhantom(int64(len(data))*16, got) != true {
		t.Log("справочно: тот же файл при заявленном размере в 16 раз больше считается фантомом")
	}
}

// Разреженный файл — то, ради чего вся проверка и написана: размер полный,
// блоков нет. Если файловая система дырки не поддерживает, тест снимается:
// проверять нечего, но и молчаливо «проходить» он не должен.
func TestAllocatedBytesSparseFile(t *testing.T) {
	const size = 64 << 20
	path := filepath.Join(t.TempDir(), "sparse")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := allocatedBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if got >= size {
		t.Skipf("файловая система не делает файлы разреженными (%d байт занято при размере %d)", got, size)
	}
	if !isPhantom(size, got) {
		t.Errorf("разреженный файл на %d байт занимает %d и не признан фантомом", size, got)
	}
}

func TestAllocatedBytesMissingFile(t *testing.T) {
	got, err := allocatedBytes(filepath.Join(t.TempDir(), "нет-такого"))
	if err != nil {
		t.Fatalf("отсутствующий файл должен давать ноль, а не ошибку: %v", err)
	}
	if got != 0 {
		t.Errorf("отсутствующий файл занимает %d байт, ожидалось 0", got)
	}
}
