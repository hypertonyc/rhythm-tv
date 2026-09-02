package mediasource

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Тесты про то, каким путём хранилище читает и пишет файлы (fileio.go).
//
// Как и в partfile_test.go, первый тест ФИКСИРУЕТ ПОВЕДЕНИЕ АПСТРИМА: он
// утверждает, что умолчание (mmap) держит удалённый файл, и упадёт, если
// anacrolix это когда-нибудь починит. Без него второй проверял бы выдумку.
//
// Оба гоняются подпроцессом, и иначе никак: реализация выбирается один раз
// на процесс, в init() пакета storage по переменной окружения, — то есть
// до первой строки любого теста.

// ioProbeEnv включает вспомогательный тест в подпроцессе.
const ioProbeEnv = "RTV_FILE_IO_PROBE"

const (
	probeHeld     = "PROBE: файл удерживается"
	probeReleased = "PROBE: файл отпущен"
)

// TestHelperReadAfterDelete скачивает файл, удаляет его с диска и пробует
// прочитать глазами хранилища. Печатает, чем кончилось; проверяют вызывающие.
func TestHelperReadAfterDelete(t *testing.T) {
	if os.Getenv(ioProbeEnv) == "" {
		t.Skip("вспомогательный тест, его запускают подпроцессом")
	}

	f := newPartFileFixture(t, newStore)
	f.downloadA(t)

	ti := f.open(t)
	// Чтение ДО удаления обязательно: у mmap-пути отображение создаётся при
	// первом обращении, а нам нужно ровно то состояние, в котором чистка
	// застаёт только что просмотренную серию.
	if got := f.readWholeA(t, ti); !bytes.Equal(got, f.dataA) {
		t.Fatalf("хранилище отдало не то, что скачано, ещё до удаления")
	}

	// Ровно то, что делает выселение: файл убирается с диска целиком
	// (mediasource.DropFile, internal/reclaim).
	if err := os.Remove(f.aPath); err != nil {
		t.Fatal(err)
	}

	mp := f.info.Piece(0)
	buf := make([]byte, mp.Length())
	_, err := ti.Piece(mp).ReadAt(buf, 0)
	switch {
	case err != nil:
		fmt.Printf("%s: %v\n", probeReleased, err)
	case bytes.Equal(buf, f.dataA[:len(buf)]):
		fmt.Println(probeHeld + ": хранилище отдало данные удалённого файла")
	default:
		fmt.Println(probeReleased + ": хранилище отдало не данные файла")
	}
}

// runReadAfterDelete гоняет вспомогательный тест с заданной реализацией
// ввода-вывода и отдаёт его вывод.
func runReadAfterDelete(t *testing.T, fileIo string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperReadAfterDelete$", "-test.v")
	cmd.Env = append(os.Environ(),
		ioProbeEnv+"=1",
		storageFileIoEnv+"="+fileIo,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("подпроцесс с %s=%s: %v\n%s", storageFileIoEnv, fileIo, err, out)
	}
	return string(out)
}

// TestUpstreamMmapStoreHoldsDeletedFiles фиксирует умолчание апстрима: файл,
// удалённый с диска, продолжает читаться из хранилища, потому что отображение
// живёт до конца процесса («the store never relinquishes its extra ref»).
//
// Для сервера это значит, что выселение не освобождает НИ ОДНОГО байта: место
// вернётся только с перезапуском. Так 02.09.2026 диск на VPS дошёл до нуля.
func TestUpstreamMmapStoreHoldsDeletedFiles(t *testing.T) {
	out := runReadAfterDelete(t, "mmap")
	if !strings.Contains(out, probeHeld) {
		t.Fatalf("АПСТРИМ ИСПРАВЛЕН: mmap-хранилище отпустило удалённый файл; перечитать fileio.go\n%s", out)
	}
}

// TestClassicStoreReleasesDeletedFiles — то же самое на классическом пути:
// данных удалённого файла хранилище не отдаёт. Проверяется этим именно то,
// что нужно: раз за данными оно идёт на диск по имени, а не в собственное
// отображение, держать inode удалённого файла ему нечем — значит блоки
// освобождаются тогда же, когда чистка зовёт os.Remove.
func TestClassicStoreReleasesDeletedFiles(t *testing.T) {
	out := runReadAfterDelete(t, classicFileIo)
	if !strings.Contains(out, probeReleased) {
		t.Fatalf("классический путь удержал удалённый файл — чистка места опять ничего не освободит\n%s", out)
	}
}

// TestImageAsksForClassicFileIo держит вторую половину решения: выбрать путь
// ввода-вывода из кода нельзя (см. fileio.go), поэтому переменная обязана
// стоять в образе — иначе любой запуск, кроме прод-compose, тихо вернётся
// к mmap, и чистка места перестанет работать.
func TestImageAsksForClassicFileIo(t *testing.T) {
	for _, path := range []string{"../../Dockerfile", "../../../deploy/compose.yaml"} {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			// deploy/ лежит вне модуля: если сервер вынули отдельно,
			// проверять нечего.
			t.Logf("%s: нет в чекауте, пропускаем", path)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), storageFileIoEnv) {
			t.Fatalf("%s: нет %s — хранилище поедет через mmap и чистка места ничего не освободит; см. internal/mediasource/fileio.go",
				path, storageFileIoEnv)
		}
		if !strings.Contains(string(body), classicFileIo) {
			t.Fatalf("%s: %s задана не в %q", path, storageFileIoEnv, classicFileIo)
		}
	}
}
