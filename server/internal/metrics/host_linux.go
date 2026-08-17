//go:build linux

package metrics

import (
	"os"
	"strconv"
)

// Чтение /proc. Единственный файл, который не собирается под macOS, поэтому
// в нём нет ничего, кроме открытия файлов: весь разбор — в parse.go.

func selfPID() int { return os.Getpid() }

// readHost снимает нагрузку машины ЦЕЛИКОМ, а не контейнера.
//
// Это осознанно: в контейнере /proc принадлежит хосту, и показания получаются
// про VPS. Дашборд для того и нужен — «хватает ли машине» важнее, чем
// «сколько съел контейнер», а контейнер здесь всё равно один.
func readHost() (Host, cpuTimes, bool) {
	statRaw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Host{}, cpuTimes{}, false
	}
	cpu, err := parseProcStat(string(statRaw))
	if err != nil {
		return Host{}, cpuTimes{}, false
	}

	out := Host{}
	if memRaw, err := os.ReadFile("/proc/meminfo"); err == nil {
		if mem, err := parseMeminfo(string(memRaw)); err == nil {
			out.MemTotal = mem.total
			out.MemAvailable = mem.available
			out.MemUsed = mem.total - mem.available
			out.MemCached = mem.cached
			out.SwapTotal = mem.swapTotal
			out.SwapUsed = mem.swapTotal - mem.swapFree
		}
	}
	if loadRaw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if l1, l5, l15, err := parseLoadavg(string(loadRaw)); err == nil {
			out.Load1, out.Load5, out.Load15 = &l1, &l5, &l15
		}
	}
	return out, cpu, true
}

// readProc снимает потребление одного процесса. Второе значение — сырые тики:
// проценты считаются по разнице, и предыдущий замер хранит вызывающий.
func readProc(pid int) (Proc, procTimes, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	statRaw, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return Proc{}, procTimes{}, false
	}
	times, err := parsePidStat(string(statRaw))
	if err != nil {
		return Proc{}, procTimes{}, false
	}
	out := Proc{PID: pid}
	if memRaw, err := os.ReadFile(dir + "/statm"); err == nil {
		if rss, err := parseStatm(string(memRaw), int64(os.Getpagesize())); err == nil {
			out.RSS = rss
		}
	}
	return out, times, true
}
