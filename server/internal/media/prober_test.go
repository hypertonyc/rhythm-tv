package media

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeProber отдаёт настоящий вывод ffprobe из testdata и считает запуски.
// Считает в файле, а не в памяти: Prober запускает внешний процесс.
func fakeProber(t *testing.T) (*Prober, func() int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffprobe — shell-скрипт")
	}

	payload, err := os.ReadFile(filepath.Join("testdata", "probe_multiaudio.json"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(dir, "runs")
	binary := filepath.Join(dir, "ffprobe-fake")
	script := "#!/bin/sh\necho x >> " + runs + "\ncat " + out + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	count := func() int {
		data, err := os.ReadFile(runs)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}

	p := &Prober{
		Binary: binary,
		RawURL: func(index int) string { return "http://127.0.0.1:0/raw/" + strconv.Itoa(index) },
	}
	return p, count
}

func TestProberCachesSuccess(t *testing.T) {
	p, runs := fakeProber(t)
	req := Request{Index: 7, Name: "S01E01.mkv"}

	for i := 0; i < 3; i++ {
		if _, err := p.Probe(context.Background(), req); err != nil {
			t.Fatalf("разбор %d: %v", i, err)
		}
	}
	if got := runs(); got != 1 {
		t.Errorf("ffprobe запускался %d раз(а), ожидался один: кэш не держит", got)
	}
}

// Forget существует ради фантомных файлов: по нулям ffprobe возвращает
// не ошибку, а правдоподобный мусор, и без сброса он оставался бы в кэше
// навсегда — телевизор так и показывал бы «Subtitles: off» после починки.
func TestProberForgetDropsEntry(t *testing.T) {
	p, runs := fakeProber(t)
	req := Request{Index: 7, Name: "S01E01.mkv"}

	if _, err := p.Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	p.Forget(7)
	if _, err := p.Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два: Forget не сбросил разбор", got)
	}

	// Соседний индекс сбрасывать не должно.
	other := Request{Index: 8, Name: "S01E02.mkv"}
	if _, err := p.Probe(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	p.Forget(7)
	if _, err := p.Probe(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 3 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось три: Forget задел чужой индекс", got)
	}
}

// Forget на неизвестном индексе и на ещё не использованном Prober — не паника.
func TestProberForgetUnknown(t *testing.T) {
	p := &Prober{}
	p.Forget(1)
}
