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

// Кэш ключуется файлом, а не индексом: активный торрент переключают
// с телефона, и один и тот же индекс после переключения означает другую серию.
// 20.08.2026 на проде это выглядело так: /api/files под индексом 159 отдавал
// «S08E01 - The Locomotion Interruption.mp4», а /api/probe/159 — «s05e11 -
// The One with All the Resolutions.mkv» с дорожками и внешними субтитрами
// «Друзей», то есть картинка шла от одного сериала, а имя, длительность
// и субтитры — от другого.
func TestProberKeepsTorrentsApart(t *testing.T) {
	p, runs := fakeProber(t)
	friends := Request{Scope: "Друзья/s05e11.mkv", Index: 159, Name: "s05e11.mkv"}
	tbbt := Request{Scope: "Big Bang Theory/S08E01.mp4", Index: 159, Name: "S08E01.mp4"}

	first, err := p.Probe(context.Background(), friends)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Probe(context.Background(), tbbt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "s05e11.mkv" || second.Name != "S08E01.mp4" {
		t.Errorf("разбор отдал %q и %q: кэш перепутал торренты", first.Name, second.Name)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два: один индекс в двух торрентах — два файла", got)
	}

	// А тот же файл по-прежнему разбирается один раз.
	if _, err := p.Probe(context.Background(), friends); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два: кэш не держит свой же файл", got)
	}
}

// Forget существует ради фантомных файлов: по нулям ffprobe возвращает
// не ошибку, а правдоподобный мусор, и без сброса он оставался бы в кэше
// навсегда — телевизор так и показывал бы «Subtitles: off» после починки.
func TestProberForgetDropsEntry(t *testing.T) {
	p, runs := fakeProber(t)
	req := Request{Scope: "сериал/S01E01.mkv", Index: 7, Name: "S01E01.mkv"}

	if _, err := p.Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	p.Forget(req.Scope)
	if _, err := p.Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два: Forget не сбросил разбор", got)
	}

	// Соседнюю серию сбрасывать не должно.
	other := Request{Scope: "сериал/S01E02.mkv", Index: 8, Name: "S01E02.mkv"}
	if _, err := p.Probe(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	p.Forget(req.Scope)
	if _, err := p.Probe(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 3 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось три: Forget задел чужой файл", got)
	}
}

// Forget на неизвестном файле и на ещё не использованном Prober — не паника.
func TestProberForgetUnknown(t *testing.T) {
	p := &Prober{}
	p.Forget("нет такого/файла.mkv")
}

// Источник без путей в хранилище — это Fake из тестов: там торрент один,
// и ключом остаётся индекс. Иначе все файлы разом схлопнулись бы в один
// пустой ключ, и разбор первой серии уехал бы во все остальные.
func TestProberWithoutScopeFallsBackToIndex(t *testing.T) {
	p, runs := fakeProber(t)

	first, err := p.Probe(context.Background(), Request{Index: 0, Name: "S01E01.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Probe(context.Background(), Request{Index: 1, Name: "S01E02.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "S01E01.mkv" || second.Name != "S01E02.mkv" {
		t.Errorf("разбор отдал %q и %q: пустой Scope склеил разные файлы", first.Name, second.Name)
	}
	if got := runs(); got != 2 {
		t.Errorf("ffprobe запускался %d раз(а), ожидалось два", got)
	}
}
