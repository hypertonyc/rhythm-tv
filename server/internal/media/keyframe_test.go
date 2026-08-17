package media

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Настоящий вывод ffprobe с прода (s03e23 «Друзей», окно 175%196): пакеты
// подряд, ключевые среди них редкие. Флаги — вторая колонка, K означает
// ключевой кадр, подчёркивание — его отсутствие.
const ffprobePackets = `175.008000,__
178.053000,K_
180.055000,__
186.186000,K_
190.190000,__
196.196000,K_
`

func TestLastKeyframeAtOrBefore(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		start float64
		want  float64
		found bool
	}{
		{
			name: "берётся последний ключевой не позже точки",
			out:  ffprobePackets, start: 195, want: 186.186, found: true,
		},
		{
			// ffprobe отдаёт интервал целиком, и хвост за точкой в нём есть
			// всегда: seek идёт к ключевому кадру, а останов — по концу
			// интервала, который сам ключевым не обязан быть.
			name: "кадр за точкой не берётся",
			out:  ffprobePackets, start: 196.195, want: 186.186, found: true,
		},
		{
			name: "точное попадание в ключевой кадр",
			out:  ffprobePackets, start: 186.186, want: 186.186, found: true,
		},
		{
			name: "ключевых кадров до точки нет",
			out:  ffprobePackets, start: 100, found: false,
		},
		{
			name:  "порядок не предполагается",
			out:   "196.196000,K_\n186.186000,K_\n",
			start: 195, want: 186.186, found: true,
		},
		{
			// У пакетов без временной метки ParseFloat падает, и это не повод
			// бросить разбор: соседние строки годны.
			name:  "N/A вместо метки пропускается",
			out:   "N/A,K_\n186.186000,K_\n",
			start: 195, want: 186.186, found: true,
		},
		{name: "пустой вывод", out: "", start: 195, found: false},
		{name: "мусор без запятой", out: "ffprobe version 5.1\n", start: 195, found: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, found := lastKeyframeAtOrBefore(c.out, c.start)
			if found != c.found {
				t.Fatalf("found = %v, ожидалось %v", found, c.found)
			}
			if found && got != c.want {
				t.Errorf("получено %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// TestKeyframeFinderBefore — проводка до ffprobe: что окно берётся ПЕРЕД точкой
// и что просматривается именно тот поток, который будет копироваться.
// Ошибиться здесь легко и незаметно: не тот -select_streams даст ключевые кадры
// чужой дорожки, а перепутанные границы интервала — кадр за точкой, то есть
// пропуск куска серии вместо повтора.
func TestKeyframeFinderBefore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffprobe — shell-скрипт")
	}
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	binary := filepath.Join(dir, "ffprobe-fake")
	script := "#!/bin/sh\necho \"$@\" > " + argv + "\nprintf '%s' '" + ffprobePackets + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &KeyframeFinder{
		RawURL:  func(i int) string { return "http://127.0.0.1:8000/raw/42" },
		Binary:  binary,
		Timeout: 5 * time.Second,
	}

	got, ok := f.Before(context.Background(), 42, 3, 195)
	if !ok {
		t.Fatal("ключевой кадр не найден")
	}
	if got != 186.186 {
		t.Errorf("получено %v, ожидалось 186.186", got)
	}

	line, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	args := string(line)
	if !strings.Contains(args, "-select_streams 3") {
		t.Errorf("просматривается не тот поток: %s", args)
	}
	if !strings.Contains(args, "-read_intervals 175.000%195.000") {
		t.Errorf("окно не перед точкой: %s", args)
	}
	if !strings.Contains(args, "http://127.0.0.1:8000/raw/42") {
		t.Errorf("читается не через /raw: %s", args)
	}
}

// TestKeyframeFinderFailuresAreNotErrors — не нашли значит перекодируем.
// Ни одна из этих неудач не должна валить сеанс: до 18.08.2026 перемотка
// перекодировалась ВСЕГДА, и откат к этому поведению — рабочий исход.
func TestKeyframeFinderFailuresAreNotErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffprobe — shell-скрипт")
	}
	rawURL := func(int) string { return "http://127.0.0.1:8000/raw/0" }

	if _, ok := (&KeyframeFinder{RawURL: rawURL, Binary: "false"}).
		Before(context.Background(), 0, 0, 195); ok {
		t.Error("упавший ffprobe обязан означать «не нашли»")
	}

	// Начало файла: ключевой кадр там есть сам собой, спрашивать нечего.
	if _, ok := (&KeyframeFinder{RawURL: rawURL, Binary: "false"}).
		Before(context.Background(), 0, 0, 0); ok {
		t.Error("нулевая точка не требует поиска")
	}

	if _, ok := (*KeyframeFinder)(nil).Before(context.Background(), 0, 0, 195); ok {
		t.Error("nil-искатель обязан молчать, а не падать")
	}

	dir := t.TempDir()
	slow := filepath.Join(dir, "ffprobe-slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &KeyframeFinder{RawURL: rawURL, Binary: slow, Timeout: 100 * time.Millisecond}
	if _, ok := f.Before(context.Background(), 0, 0, 195); ok {
		t.Error("таймаут обязан означать «не нашли»")
	}
}
