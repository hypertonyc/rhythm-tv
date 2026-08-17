// Package metrics — показания дашборда: нагрузка машины и внутренние счётчики
// сервера, снятые регулярно и сложенные в кольцо на последние минуты.
//
// Отдельный пакет по той же причине, что и остальные здесь, — по опасности,
// а не по существительному. Это ЕДИНСТВЕННОЕ место, где сервер смотрит
// на операционную систему помимо собственных файлов: /proc, loadavg, statfs.
// Всё это либо текст чужого формата, либо системный вызов, которого на macOS
// нет вовсе, поэтому разбор вынесен в чистые функции и покрыт тестами
// на образцах: иначе проверить его можно было бы только выкаткой.
//
// Наружу пакет отдаёт СВОИ типы, а не hls.Snapshot и не reclaim.Snapshot,
// хотя числа берёт оттуда. Так дашборд не приколачивается к контракту
// с телевизором: /api/status заморожен побайтово, и любое желание показать
// на телефоне ещё одну цифру не должно даже приближаться к нему.
package metrics

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"
)

const (
	defaultInterval = 2 * time.Second
	defaultWindow   = 10 * time.Minute
	// Каталоги на диске обходятся куда реже опроса: это единственная тяжёлая
	// операция здесь, а «сколько занято скачанным» меняется медленно.
	usageInterval = time.Minute
)

// noValue — «не измерено» внутри кольца.
//
// Отдельного флага на точку нет намеренно: все ряды здесь неотрицательны
// (проценты, байты, скорости), поэтому -1 не может быть настоящим показанием,
// а кольцо остаётся плоским массивом чисел вместо массива указателей.
// Наружу такая точка уходит как null — в графике это разрыв, а не ноль,
// и разница видна: ноль означает «ffmpeg не производит ничего», null —
// «в эту секунду сеанса не было вовсе».
const noValue = -1

// Torrent — живые счётчики активного торрента.
type Torrent struct {
	Name          string  `json:"name"`
	Peers         int     `json:"peers"`
	DownloadSpeed float64 `json:"downloadSpeed"`
	Downloaded    int64   `json:"downloaded"`
	Progress      float64 `json:"progress"`
}

// Session — то немногое из снимка сеанса, что нужно дашборду.
//
// Полный снимок лежит в /api/status, и экран сеанса берёт его оттуда;
// здесь только то, из чего считаются ряды и плитки.
type Session struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Segments  int    `json:"segments"`
	BytesOut  int64  `json:"bytesOut"`
	StartedAt int64  `json:"startedAt"`
	VideoMode string `json:"videoMode"`
	AudioMode string `json:"audioMode"`
	PID       *int   `json:"ffmpegPid"`
}

// Reclaim — итог работы чистки места.
type Reclaim struct {
	MinFree       int64  `json:"minFree"`
	TargetFree    int64  `json:"targetFree"`
	Evicted       int    `json:"evicted"`
	EvictedBytes  int64  `json:"evictedBytes"`
	LastEvictedAt *int64 `json:"lastEvictedAt"`
}

// Host — нагрузка машины целиком.
//
// Measured отделяет «не умеем мерить на этой системе» от «намерили ноль»:
// под macOS /proc нет, и страница обязана сказать об этом прямо, а не рисовать
// плоскую линию на нуле, будто сервер простаивает.
type Host struct {
	Measured bool `json:"measured"`
	Cores    int  `json:"cores"`

	CPU    *float64 `json:"cpu"`
	Load1  *float64 `json:"load1"`
	Load5  *float64 `json:"load5"`
	Load15 *float64 `json:"load15"`

	MemTotal     int64 `json:"memTotal"`
	MemAvailable int64 `json:"memAvailable"`
	MemUsed      int64 `json:"memUsed"`
	MemCached    int64 `json:"memCached"`
	SwapTotal    int64 `json:"swapTotal"`
	SwapUsed     int64 `json:"swapUsed"`
}

// Proc — потребление одного процесса: нашего или ffmpeg.
type Proc struct {
	PID int      `json:"pid"`
	CPU *float64 `json:"cpu"`
	RSS int64    `json:"rss"`
}

// Runtime — счётчики самого сервера.
type Runtime struct {
	Goroutines int     `json:"goroutines"`
	HeapAlloc  int64   `json:"heapAlloc"`
	HeapSys    int64   `json:"heapSys"`
	NumGC      uint32  `json:"numGC"`
	Uptime     float64 `json:"uptime"`
}

// Disk — одна файловая система.
//
// Free — доступное НАМ (Bavail), а Used — занятое всеми (Blocks-Bfree),
// поэтому Free+Used меньше Total на резерв под root. Это не ошибка расчёта:
// именно по Free принимает решение чистка, и показывать надо то же число.
type Disk struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
	Used  int64  `json:"used"`
	// Ours — сколько на этой ФС занимаем мы (скачанное или сегменты);
	// -1 означает «ещё не считали» или «посчитать не удалось».
	Ours int64 `json:"ours"`
}

// Rates — производные скорости, которых нет ни в одном счётчике напрямую.
type Rates struct {
	Output   *float64 `json:"output"`
	Segments *float64 `json:"segments"`
}

// Series — ряды за окно наблюдения, колонками.
//
// Колонками, а не массивом объектов: имена полей иначе повторились бы
// в каждой точке, и ответ вырос бы вчетверо на ровном месте. Индексы
// во всех рядах общие, T задаёт время.
type Series struct {
	T        []int64    `json:"t"`
	CPU      []*float64 `json:"cpu"`
	Mem      []*float64 `json:"mem"`
	Free     []*float64 `json:"free"`
	Download []*float64 `json:"download"`
	Output   []*float64 `json:"output"`
	FFmpeg   []*float64 `json:"ffmpeg"`
	Peers    []*float64 `json:"peers"`
}

// Report — то, что уезжает в /api/metrics.
type Report struct {
	At      int64    `json:"at"`
	Host    Host     `json:"host"`
	Process Proc     `json:"process"`
	FFmpeg  *Proc    `json:"ffmpeg"`
	Runtime Runtime  `json:"runtime"`
	Disks   []Disk   `json:"disks"`
	Torrent *Torrent `json:"torrent"`
	Session *Session `json:"session"`
	Reclaim *Reclaim `json:"reclaim"`
	Rates   Rates    `json:"rates"`
	// Series приезжает только по запросу: это десяток килобайт, и опрашивать
	// их раз в две секунды незачем — страница дорисовывает свежую точку сама.
	Series *Series `json:"series"`
}

// Options — источники живых чисел.
//
// Все функции необязательны: без них соответствующее поле уезжает null,
// и страница показывает прочерк. Так пакет собирается и тестируется
// без торрента, без ffmpeg и без чистки.
type Options struct {
	Interval time.Duration
	Window   time.Duration

	// StoreDir — куда качается торрент, HLSDir — где лежат каталоги сеансов.
	// Обычно это разные файловые системы только на машине разработчика.
	StoreDir string
	HLSDir   string

	Torrent func() *Torrent
	Session func() *Session
	Reclaim func() *Reclaim

	Now func() time.Time
	// Читатели системы подменяются в тестах: настоящих /proc и statfs там нет,
	// а под macOS нет и вовсе. Сырые счётчики времени процессора уезжают
	// наружу вторым значением, потому что проценты считаются по разнице
	// и хранить предыдущий замер обязан вызывающий.
	host     func() (Host, cpuTimes, bool)
	proc     func(pid int) (Proc, procTimes, bool)
	disk     func(path string) (total, free, used int64, ok bool)
	dirUsage func(path string) (int64, bool)
}

// sample — одна точка кольца. Значения -1 означают «не измерено».
type sample struct {
	at       int64
	cpu      float64
	mem      float64
	free     float64
	download float64
	output   float64
	ffmpeg   float64
	peers    float64
}

// Collector снимает показания по таймеру и держит окно истории.
//
// Всё состояние под одним мьютексом — ровно по тем же соображениям, что
// и в hls.Manager: порядок блокировок как класс убран, а нагрузка тут —
// один тик в две секунды.
type Collector struct {
	opts Options

	mu   sync.Mutex
	ring []sample
	head int
	size int

	last Report

	startedAt time.Time
	prevAt    time.Time
	prevCPU   cpuTimes
	// prevSelfOK и prevFFPID отвечают на вопрос «был ли предыдущий замер».
	// Нулевые тики этим признаком быть не могут: у только что запущенного
	// ffmpeg их и правда ноль, и по ним первая же секунда работы потерялась бы.
	prevSelf   procTimes
	prevSelfOK bool
	prevFF     procTimes
	prevFFPID  int

	prevSession  string
	prevBytesOut int64
	prevSegments int

	usage map[string]int64
}

// New собирает сборщик. Run надо запустить отдельно.
func New(opts Options) *Collector {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Window <= 0 {
		opts.Window = defaultWindow
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.host == nil {
		opts.host = readHost
	}
	if opts.proc == nil {
		opts.proc = readProc
	}
	if opts.disk == nil {
		opts.disk = diskUsage
	}
	if opts.dirUsage == nil {
		opts.dirUsage = dirUsage
	}
	points := int(opts.Window / opts.Interval)
	if points < 2 {
		points = 2
	}
	now := opts.Now()
	c := &Collector{
		opts:      opts,
		ring:      make([]sample, points),
		startedAt: now,
		usage:     make(map[string]int64),
	}
	// Первый замер сразу: иначе страница, открытая через секунду после старта,
	// две секунды показывала бы пустоту вместо цифр.
	c.collect()
	return c
}

// Run снимает показания, пока жив контекст.
func (c *Collector) Run(ctx context.Context) {
	tick := time.NewTicker(c.opts.Interval)
	defer tick.Stop()
	usageTick := time.NewTicker(usageInterval)
	defer usageTick.Stop()

	// Обход каталогов — единственное тяжёлое место здесь, и делать его
	// в такте опроса нельзя: на большом хранилище он занял бы заметно больше
	// интервала, и ряды поехали бы. Отдельная горутина, отдельный ритм.
	go c.measureUsage()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.collect()
		case <-usageTick.C:
			go c.measureUsage()
		}
	}
}

// Report отдаёт последний снимок; history=true добавляет ряды за окно.
func (c *Collector) Report(history bool) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.last
	if history {
		out.Series = c.seriesLocked()
	}
	return out
}

// collect снимает всё разом и кладёт точку в кольцо.
func (c *Collector) collect() {
	now := c.opts.Now()

	host, cpu, hostOK := c.opts.host()
	host.Cores = runtime.NumCPU()
	host.Measured = hostOK

	// PID проставляется независимо от того, удалось ли снять показания:
	// на системе без /proc это единственное, что мы про себя знаем наверняка,
	// и «pid 0» на экране выглядело бы ошибкой, а не отсутствием замера.
	self, selfTimes, selfOK := c.opts.proc(selfPID())
	self.PID = selfPID()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var torrent *Torrent
	if c.opts.Torrent != nil {
		torrent = c.opts.Torrent()
	}
	var session *Session
	if c.opts.Session != nil {
		session = c.opts.Session()
	}
	var recl *Reclaim
	if c.opts.Reclaim != nil {
		recl = c.opts.Reclaim()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	dt := now.Sub(c.prevAt).Seconds()
	if c.prevAt.IsZero() || dt <= 0 {
		dt = 0
	}

	// Проценты процессора считаются только по РАЗНИЦЕ: одного замера мало,
	// /proc/stat и /proc/<pid>/stat хранят время с загрузки машины.
	if hostOK {
		host.CPU = cpuPercent(c.prevCPU, cpu)
		c.prevCPU = cpu
	}
	if selfOK && dt > 0 && c.prevSelfOK {
		self.CPU = procPercent(c.prevSelf, selfTimes, dt)
	}
	if selfOK {
		c.prevSelf, c.prevSelfOK = selfTimes, true
	}

	var ffmpeg *Proc
	if session != nil && session.PID != nil {
		p, times, ok := c.opts.proc(*session.PID)
		if ok {
			if dt > 0 && c.prevFFPID == *session.PID {
				p.CPU = procPercent(c.prevFF, times, dt)
			}
			c.prevFF, c.prevFFPID = times, *session.PID
			ffmpeg = &p
		}
	}
	if ffmpeg == nil {
		c.prevFFPID = 0
	}

	disks := c.disksLocked()

	// Скорость выдачи ffmpeg считается по тому же сеансу: при замене сеанса
	// счётчики начинаются заново, и разница со старыми дала бы всплеск вниз.
	var rates Rates
	if session != nil && dt > 0 && session.ID == c.prevSession {
		rates.Output = ratePtr(float64(session.BytesOut-c.prevBytesOut) / dt)
		rates.Segments = ratePtr(float64(session.Segments-c.prevSegments) / dt)
	}
	if session != nil {
		c.prevSession, c.prevBytesOut, c.prevSegments = session.ID, session.BytesOut, session.Segments
	} else {
		c.prevSession, c.prevBytesOut, c.prevSegments = "", 0, 0
	}

	c.prevAt = now
	c.last = Report{
		At:      now.UnixMilli(),
		Host:    host,
		Process: self,
		FFmpeg:  ffmpeg,
		Runtime: Runtime{
			Goroutines: runtime.NumGoroutine(),
			HeapAlloc:  int64(mem.HeapAlloc),
			HeapSys:    int64(mem.HeapSys),
			NumGC:      mem.NumGC,
			Uptime:     now.Sub(c.startedAt).Seconds(),
		},
		Disks:   disks,
		Torrent: torrent,
		Session: session,
		Reclaim: recl,
		Rates:   rates,
	}

	c.pushLocked(sample{
		at:       now.UnixMilli(),
		cpu:      value(host.CPU),
		mem:      memPoint(host),
		free:     freePoint(disks),
		download: downloadPoint(torrent),
		output:   value(rates.Output),
		ffmpeg:   ffmpegPoint(ffmpeg),
		peers:    peersPoint(torrent),
	})
}

// disksLocked собирает файловые системы, на которых мы живём.
//
// Одинаковые ФС схлопываются: на проде хранилище и сегменты HLS лежат
// на одном разделе, и две одинаковых полоски на экране только сбивали бы
// с толку. Признак «та же ФС» — совпадение размера и свободного места:
// точнее было бы по fsid, но его поля разъезжаются между Linux и macOS,
// а ошибка здесь стоит одной лишней строки на экране.
func (c *Collector) disksLocked() []Disk {
	out := make([]Disk, 0, 2)
	add := func(label, path string) {
		if path == "" {
			return
		}
		total, free, used, ok := c.opts.disk(path)
		if !ok {
			return
		}
		for i := range out {
			if out[i].Total == total && out[i].Free == free {
				out[i].Label += "+" + label
				if ours, seen := c.usage[label]; seen && out[i].Ours >= 0 {
					out[i].Ours += ours
				}
				return
			}
		}
		ours := int64(noValue)
		if v, seen := c.usage[label]; seen {
			ours = v
		}
		out = append(out, Disk{Label: label, Path: path, Total: total, Free: free, Used: used, Ours: ours})
	}
	add("store", c.opts.StoreDir)
	add("hls", c.opts.HLSDir)
	return out
}

// measureUsage обходит каталоги и запоминает, сколько мы занимаем.
func (c *Collector) measureUsage() {
	measured := make(map[string]int64, 2)
	if c.opts.StoreDir != "" {
		if n, ok := c.opts.dirUsage(c.opts.StoreDir); ok {
			measured["store"] = n
		}
	}
	if c.opts.HLSDir != "" {
		if n, ok := sessionsUsage(c.opts.HLSDir, c.opts.dirUsage); ok {
			measured["hls"] = n
		}
	}
	c.mu.Lock()
	for k, v := range measured {
		c.usage[k] = v
	}
	c.mu.Unlock()
}

func (c *Collector) pushLocked(s sample) {
	c.ring[c.head] = s
	c.head = (c.head + 1) % len(c.ring)
	if c.size < len(c.ring) {
		c.size++
	}
}

// seriesLocked разворачивает кольцо в ряды от старого к новому.
func (c *Collector) seriesLocked() *Series {
	out := &Series{
		T:        make([]int64, 0, c.size),
		CPU:      make([]*float64, 0, c.size),
		Mem:      make([]*float64, 0, c.size),
		Free:     make([]*float64, 0, c.size),
		Download: make([]*float64, 0, c.size),
		Output:   make([]*float64, 0, c.size),
		FFmpeg:   make([]*float64, 0, c.size),
		Peers:    make([]*float64, 0, c.size),
	}
	start := (c.head - c.size + len(c.ring)) % len(c.ring)
	for i := 0; i < c.size; i++ {
		s := c.ring[(start+i)%len(c.ring)]
		out.T = append(out.T, s.at)
		out.CPU = append(out.CPU, point(s.cpu))
		out.Mem = append(out.Mem, point(s.mem))
		out.Free = append(out.Free, point(s.free))
		out.Download = append(out.Download, point(s.download))
		out.Output = append(out.Output, point(s.output))
		out.FFmpeg = append(out.FFmpeg, point(s.ffmpeg))
		out.Peers = append(out.Peers, point(s.peers))
	}
	return out
}

// point переводит внутренний -1 в null для клиента.
func point(v float64) *float64 {
	if v < 0 {
		return nil
	}
	rounded := math.Round(v*100) / 100
	return &rounded
}

func value(p *float64) float64 {
	if p == nil {
		return noValue
	}
	return *p
}

// ratePtr отбрасывает отрицательные скорости: счётчики только растут,
// а минус означает, что мы сравнили разные сеансы.
func ratePtr(v float64) *float64 {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	rounded := math.Round(v*100) / 100
	return &rounded
}

func memPoint(h Host) float64 {
	if !h.Measured || h.MemTotal <= 0 {
		return noValue
	}
	return float64(h.MemUsed)
}

func freePoint(disks []Disk) float64 {
	if len(disks) == 0 {
		return noValue
	}
	return float64(disks[0].Free)
}

func downloadPoint(t *Torrent) float64 {
	if t == nil || t.DownloadSpeed < 0 {
		return noValue
	}
	return t.DownloadSpeed
}

func peersPoint(t *Torrent) float64 {
	if t == nil {
		return noValue
	}
	return float64(t.Peers)
}

func ffmpegPoint(p *Proc) float64 {
	if p == nil || p.CPU == nil {
		return noValue
	}
	return *p.CPU
}

// cpuPercent — доля занятого времени между двумя замерами /proc/stat.
func cpuPercent(prev, cur cpuTimes) *float64 {
	if prev.total == 0 || cur.total <= prev.total {
		return nil
	}
	total := float64(cur.total - prev.total)
	idle := float64(cur.idle - prev.idle)
	pct := 100 * (1 - idle/total)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	rounded := math.Round(pct*10) / 10
	return &rounded
}

// procPercent — сколько процента ОДНОГО ядра съел процесс за dt секунд.
//
// Одного ядра, а не машины: у ffmpeg с несколькими потоками бывает и 300%,
// и это осмысленное показание — ровно так его показывает top.
//
// «Был ли предыдущий замер» здесь не выясняется: это знает вызывающий,
// и знает точнее — у свежего процесса тики законно нулевые.
func procPercent(prev, cur procTimes, dt float64) *float64 {
	if cur.ticks < prev.ticks || dt <= 0 {
		return nil
	}
	pct := float64(cur.ticks-prev.ticks) / clockTicks / dt * 100
	if pct < 0 {
		return nil
	}
	rounded := math.Round(pct*10) / 10
	return &rounded
}
