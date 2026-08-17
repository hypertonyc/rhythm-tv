package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Сборщик проверяется на подставных читателях системы: настоящих /proc
// и statfs под macOS нет, а на Linux их показания меняются сами по себе
// и проверять по ним нечего.

type fakeSystem struct {
	cpu   cpuTimes
	procs map[int]procTimes
	disks map[string][3]int64 // total, free, used
}

func (f *fakeSystem) host() (Host, cpuTimes, bool) {
	return Host{MemTotal: 1000, MemAvailable: 400, MemUsed: 600}, f.cpu, true
}

func (f *fakeSystem) proc(pid int) (Proc, procTimes, bool) {
	t, ok := f.procs[pid]
	if !ok {
		return Proc{}, procTimes{}, false
	}
	return Proc{PID: pid, RSS: 1 << 20}, t, true
}

func (f *fakeSystem) disk(path string) (total, free, used int64, ok bool) {
	v, ok := f.disks[path]
	if !ok {
		return 0, 0, 0, false
	}
	return v[0], v[1], v[2], true
}

// clock — управляемые часы: интервал между замерами задаёт тест, а не таймер.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestCollector(t *testing.T, sys *fakeSystem, clk *clock, opts Options) *Collector {
	t.Helper()
	opts.host = sys.host
	opts.proc = sys.proc
	opts.disk = sys.disk
	opts.dirUsage = func(string) (int64, bool) { return 0, false }
	opts.Now = clk.now
	return New(opts)
}

// TestCPUNeedsTwoSamples — процент процессора считается по РАЗНИЦЕ, и первого
// замера для него мало: /proc/stat хранит время с загрузки машины.
func TestCPUNeedsTwoSamples(t *testing.T) {
	sys := &fakeSystem{cpu: cpuTimes{total: 1000, idle: 900}, procs: map[int]procTimes{}}
	clk := &clock{t: time.Unix(1000, 0)}
	c := newTestCollector(t, sys, clk, Options{Interval: time.Second, Window: time.Minute})

	if got := c.Report(false); got.Host.CPU != nil {
		t.Errorf("после первого замера CPU=%v, ожидался null", *got.Host.CPU)
	}

	// За секунду всего прошло 100 тиков, из них 25 в простое → занято 75%.
	sys.cpu = cpuTimes{total: 1100, idle: 925}
	clk.t = clk.t.Add(time.Second)
	c.collect()

	got := c.Report(false)
	if got.Host.CPU == nil {
		t.Fatal("после второго замера CPU всё ещё null")
	}
	if *got.Host.CPU != 75 {
		t.Errorf("CPU=%v, ожидалось 75", *got.Host.CPU)
	}
}

// TestProcPercentIsPerCore — у ffmpeg с несколькими потоками бывает и 300%,
// и это осмысленно: ровно так его показывает top. Делить на число ядер
// здесь НЕЛЬЗЯ, иначе «ffmpeg ест ядро целиком» превратится в 25%.
func TestProcPercentIsPerCore(t *testing.T) {
	pid := 4242
	sys := &fakeSystem{procs: map[int]procTimes{pid: {ticks: 0}}}
	clk := &clock{t: time.Unix(1000, 0)}
	session := &Session{ID: "s1", PID: &pid}
	c := newTestCollector(t, sys, clk, Options{
		Interval: time.Second, Window: time.Minute,
		Session: func() *Session { return session },
	})

	// 200 тиков за секунду при USER_HZ=100 — это два ядра целиком.
	sys.procs[pid] = procTimes{ticks: 200}
	clk.t = clk.t.Add(time.Second)
	c.collect()

	got := c.Report(false)
	if got.FFmpeg == nil || got.FFmpeg.CPU == nil {
		t.Fatal("ffmpeg не измерен")
	}
	if *got.FFmpeg.CPU != 200 {
		t.Errorf("ffmpeg CPU=%v, ожидалось 200", *got.FFmpeg.CPU)
	}
}

// TestRatesResetOnNewSession — счётчики сеанса начинаются с нуля, и разница
// с предыдущим сеансом дала бы отрицательную скорость выдачи. Отдавать её
// нельзя ни в каком виде: на графике это провал в минус, а в плитке — минус
// мегабайт в секунду.
func TestRatesResetOnNewSession(t *testing.T) {
	sys := &fakeSystem{procs: map[int]procTimes{}}
	clk := &clock{t: time.Unix(1000, 0)}
	session := &Session{ID: "s1", BytesOut: 1_000_000, Segments: 10}
	c := newTestCollector(t, sys, clk, Options{
		Interval: time.Second, Window: time.Minute,
		Session: func() *Session { return session },
	})

	session = &Session{ID: "s1", BytesOut: 3_000_000, Segments: 12}
	clk.t = clk.t.Add(time.Second)
	c.collect()
	if got := c.Report(false); got.Rates.Output == nil || *got.Rates.Output != 2_000_000 {
		t.Errorf("output=%v, ожидалось 2000000", got.Rates.Output)
	}

	// Новый сеанс: счётчики поехали с нуля.
	session = &Session{ID: "s2", BytesOut: 5_000, Segments: 1}
	clk.t = clk.t.Add(time.Second)
	c.collect()
	if got := c.Report(false); got.Rates.Output != nil {
		t.Errorf("после смены сеанса output=%v, ожидался null", *got.Rates.Output)
	}
}

// TestSeriesKeepsOrderAndWraps — кольцо обязано разворачиваться от старого
// к новому и после переполнения отдавать ровно окно, а не мешанину.
func TestSeriesKeepsOrderAndWraps(t *testing.T) {
	sys := &fakeSystem{cpu: cpuTimes{total: 1000, idle: 900}, procs: map[int]procTimes{}}
	clk := &clock{t: time.Unix(1000, 0)}
	// Окно на три точки.
	c := newTestCollector(t, sys, clk, Options{Interval: time.Second, Window: 3 * time.Second})

	for i := 0; i < 5; i++ {
		clk.t = clk.t.Add(time.Second)
		c.collect()
	}

	series := c.Report(true).Series
	if series == nil {
		t.Fatal("рядов нет")
	}
	if len(series.T) != 3 {
		t.Fatalf("точек %d, ожидалось 3", len(series.T))
	}
	for i := 1; i < len(series.T); i++ {
		if series.T[i] <= series.T[i-1] {
			t.Fatalf("время не растёт: %v", series.T)
		}
	}
	if series.T[2] != clk.t.UnixMilli() {
		t.Errorf("последняя точка %d, ожидалась %d", series.T[2], clk.t.UnixMilli())
	}
	if len(series.CPU) != 3 || len(series.Peers) != 3 {
		t.Errorf("ряды разной длины: cpu=%d peers=%d", len(series.CPU), len(series.Peers))
	}
}

// TestSeriesGapsAreNull — ноль и «не измерено» обязаны отличаться.
//
// Ноль в скорости выдачи означает «ffmpeg жив и не производит ничего» —
// это симптом. Отсутствие сеанса вовсе — не симптом, и рисовать его нулём
// значит показывать проблему там, где её нет.
func TestSeriesGapsAreNull(t *testing.T) {
	sys := &fakeSystem{procs: map[int]procTimes{}}
	clk := &clock{t: time.Unix(1000, 0)}
	c := newTestCollector(t, sys, clk, Options{Interval: time.Second, Window: time.Minute})
	clk.t = clk.t.Add(time.Second)
	c.collect()

	series := c.Report(true).Series
	for i, v := range series.Output {
		if v != nil {
			t.Errorf("точка %d: output=%v, а сеанса не было вовсе", i, *v)
		}
	}
	for i, v := range series.Peers {
		if v != nil {
			t.Errorf("точка %d: peers=%v, а торрента не было вовсе", i, *v)
		}
	}
}

// TestDisksCollapseSameFilesystem — на проде хранилище и сегменты HLS лежат
// на одном разделе, и две одинаковые полоски на экране только сбивали бы
// с толку; порог свободного места считается по ним обоим сразу.
func TestDisksCollapseSameFilesystem(t *testing.T) {
	sys := &fakeSystem{
		procs: map[int]procTimes{},
		disks: map[string][3]int64{
			"/store": {100, 40, 55},
			"/tmp":   {100, 40, 55},
		},
	}
	clk := &clock{t: time.Unix(1000, 0)}
	c := newTestCollector(t, sys, clk, Options{
		Interval: time.Second, Window: time.Minute,
		StoreDir: "/store", HLSDir: "/tmp",
	})

	disks := c.Report(false).Disks
	if len(disks) != 1 {
		t.Fatalf("файловых систем %d, ожидалась одна: %+v", len(disks), disks)
	}
	if disks[0].Label != "store+hls" {
		t.Errorf("label=%q", disks[0].Label)
	}
}

func TestDisksStayApartWhenDifferent(t *testing.T) {
	sys := &fakeSystem{
		procs: map[int]procTimes{},
		disks: map[string][3]int64{
			"/store": {100, 40, 55},
			"/tmp":   {200, 90, 100},
		},
	}
	clk := &clock{t: time.Unix(1000, 0)}
	c := newTestCollector(t, sys, clk, Options{
		Interval: time.Second, Window: time.Minute,
		StoreDir: "/store", HLSDir: "/tmp",
	})
	if got := c.Report(false).Disks; len(got) != 2 {
		t.Fatalf("файловых систем %d, ожидалось две: %+v", len(got), got)
	}
}

// TestHistoryOnlyOnDemand — ряды весят на два порядка больше самого снимка,
// и опрос раз в две секунды тянул бы их впустую.
func TestHistoryOnlyOnDemand(t *testing.T) {
	sys := &fakeSystem{procs: map[int]procTimes{}}
	clk := &clock{t: time.Unix(1000, 0)}
	c := newTestCollector(t, sys, clk, Options{Interval: time.Second, Window: time.Minute})
	if c.Report(false).Series != nil {
		t.Error("ряды приехали без запроса")
	}
	if c.Report(true).Series == nil {
		t.Error("ряды не приехали по запросу")
	}
}

// TestSessionsUsageSkipsForeignDirs — во временном каталоге лежит чужое,
// и «мы занимаем» не должно превращаться в «в /tmp что-то есть».
func TestSessionsUsageSkipsForeignDirs(t *testing.T) {
	seen := make([]string, 0, 2)
	walk := func(path string) (int64, bool) {
		seen = append(seen, path)
		return 10, true
	}
	tmp := t.TempDir()
	for _, name := range []string{"tms-hls-abc", "tms-hls-def", "systemd-private-x", "somebody-else"} {
		if err := mkdir(tmp, name); err != nil {
			t.Fatal(err)
		}
	}
	total, ok := sessionsUsage(tmp, walk)
	if !ok {
		t.Fatal("каталог не прочитан")
	}
	if total != 20 || len(seen) != 2 {
		t.Errorf("посчитано %d по %d каталогам: %v", total, len(seen), seen)
	}
}

func mkdir(root, name string) error { return os.Mkdir(filepath.Join(root, name), 0o755) }
