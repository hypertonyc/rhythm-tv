package metrics

import "testing"

// TestParseProcStatExcludesGuest.
//
// guest и guest_nice ядро уже посчитало в user и nice. Сложив все поля подряд,
// мы завысили бы знаменатель на время гостей, и на машине с виртуалками
// процессор показывал бы меньше, чем он занят на самом деле.
func TestParseProcStatExcludesGuest(t *testing.T) {
	got, err := parseProcStat("cpu  10 20 30 40 50 60 70 80 90 100\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatal(err)
	}
	// 10+20+30+40+50+60+70+80 = 360, простой = idle 40 + iowait 50.
	if got.total != 360 || got.idle != 90 {
		t.Errorf("total=%d idle=%d, ожидалось 360/90", got.total, got.idle)
	}
}

func TestParseProcStatRealLine(t *testing.T) {
	raw := "cpu  1279816 3412 336702 74303543 41411 0 12142 0 0 0\n" +
		"cpu0 639908 1706 168351 37151771 20705 0 6071 0 0 0\n" +
		"intr 123456\n"
	got, err := parseProcStat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 75977026 || got.idle != 74344954 {
		t.Errorf("total=%d idle=%d", got.total, got.idle)
	}
}

func TestParseProcStatWithoutCPULine(t *testing.T) {
	if _, err := parseProcStat("intr 1 2 3\nctxt 4\n"); err == nil {
		t.Error("строки cpu нет, а ошибки нет")
	}
	// «cpu0» не должен приниматься за «cpu»: это одно ядро, а не машина.
	if _, err := parseProcStat("cpu0 1 2 3 4\n"); err == nil {
		t.Error("cpu0 принят за суммарную строку")
	}
}

func TestParseMeminfo(t *testing.T) {
	raw := "MemTotal:        4028420 kB\n" +
		"MemFree:          181236 kB\n" +
		"MemAvailable:    2913064 kB\n" +
		"Buffers:          124352 kB\n" +
		"Cached:          2461288 kB\n" +
		"SwapTotal:       2097148 kB\n" +
		"SwapFree:        2091516 kB\n"
	got, err := parseMeminfo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 4028420<<10 {
		t.Errorf("total=%d", got.total)
	}
	if got.available != 2913064<<10 {
		t.Errorf("available=%d", got.available)
	}
	// Buffers и Cached складываются: на графике это одна величина.
	if got.cached != (124352+2461288)<<10 {
		t.Errorf("cached=%d", got.cached)
	}
	if got.swapTotal != 2097148<<10 || got.swapFree != 2091516<<10 {
		t.Errorf("swap=%d/%d", got.swapUsed(), got.swapTotal)
	}
}

// swapUsed нужен только тесту выше для внятного сообщения об ошибке.
func (m memInfo) swapUsed() int64 { return m.swapTotal - m.swapFree }

func TestParseMeminfoEmpty(t *testing.T) {
	if _, err := parseMeminfo("Hugepagesize:       2048 kB\n"); err == nil {
		t.Error("без MemTotal разбор обязан отказать")
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg("0.42 0.35 0.30 2/311 27182\n")
	if err != nil {
		t.Fatal(err)
	}
	if l1 != 0.42 || l5 != 0.35 || l15 != 0.30 {
		t.Errorf("%v %v %v", l1, l5, l15)
	}
}

// TestParsePidStatSurvivesWeirdComm — главный кейс этого файла.
//
// Второе поле /proc/<pid>/stat — имя исполняемого файла в скобках, и оно
// содержит что угодно: пробелы, скобки, юникод. Наивный split по пробелам
// с начала строки съезжает на столько полей, сколько в имени пробелов,
// и utime превращается в мусор — молча, без ошибки. Поэтому разбор идёт
// от ПОСЛЕДНЕЙ закрывающей скобки.
func TestParsePidStatSurvivesWeirdComm(t *testing.T) {
	for _, comm := range []string{"ffmpeg", "my (weird) prog", "a b c", "((("} {
		raw := "1234 (" + comm + ") S 1 1234 1234 0 -1 4194304 100 0 0 0 111 222 0 0 20 0 1 0 100 0 0\n"
		got, err := parsePidStat(raw)
		if err != nil {
			t.Fatalf("comm=%q: %v", comm, err)
		}
		if got.ticks != 111+222 {
			t.Errorf("comm=%q: ticks=%d, ожидалось %d", comm, got.ticks, 111+222)
		}
	}
}

func TestParsePidStatShort(t *testing.T) {
	if _, err := parsePidStat("1234 (ffmpeg) S 1 2 3\n"); err == nil {
		t.Error("обрезанная строка принята")
	}
	if _, err := parsePidStat("мусор без скобок"); err == nil {
		t.Error("строка без скобок принята")
	}
}

func TestParseStatm(t *testing.T) {
	got, err := parseStatm("21858 5460 3134 1 0 3268 0\n", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5460*4096 {
		t.Errorf("rss=%d", got)
	}
}
