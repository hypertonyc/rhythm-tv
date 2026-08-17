package metrics

import (
	"errors"
	"strconv"
	"strings"
)

// Разбор текстовых форматов /proc.
//
// Чистые функции над строкой, а не над файлом, намеренно: под macOS /proc нет,
// и без этого разделения проверить разбор можно было бы только выкаткой
// на VPS. Здесь же он покрыт образцами с настоящей машины — включая те,
// на которых наивный split ломается (см. parsePidStat).

// clockTicks — USER_HZ, единица времени в /proc/<pid>/stat.
//
// Константа, а не sysconf(_SC_CLK_TCK): точное значение достаётся только
// через cgo, а на Linux оно равно 100 на всех архитектурах, где ядро вообще
// собирают под этот проект. Ошибка в этой константе видна сразу — проценты
// процессора уехали бы кратно.
const clockTicks = 100.0

var errNoField = errors.New("нужного поля нет")

// cpuTimes — суммарное и простойное время процессора с загрузки машины.
type cpuTimes struct {
	total uint64
	idle  uint64
}

// procTimes — время процессора одного процесса в тиках.
type procTimes struct {
	ticks uint64
}

// parseProcStat читает первую строку /proc/stat («cpu ...»).
//
// guest и guest_nice в сумму НЕ входят: ядро уже посчитало их в user и nice,
// и сложив всё подряд, мы завысили бы знаменатель ровно на время гостей.
func parseProcStat(s string) (cpuTimes, error) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu ") && !strings.HasPrefix(line, "cpu\t") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 4 {
			return cpuTimes{}, errNoField
		}
		var out cpuTimes
		for i, f := range fields {
			// user nice system idle iowait irq softirq steal guest guest_nice
			if i > 7 {
				break
			}
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			out.total += v
			if i == 3 || i == 4 {
				out.idle += v
			}
		}
		return out, nil
	}
	return cpuTimes{}, errNoField
}

// memInfo — то, что нужно из /proc/meminfo, уже в байтах.
type memInfo struct {
	total     int64
	available int64
	cached    int64
	swapTotal int64
	swapFree  int64
}

// parseMeminfo разбирает /proc/meminfo.
//
// MemAvailable, а не MemFree: свободной памяти на живой машине почти нет,
// её съедает страничный кэш, и по MemFree дашборд вечно показывал бы
// «памяти не осталось». MemAvailable — оценка ядра, сколько можно занять
// без свопа, то есть ровно то, что человек называет свободной памятью.
// Buffers складываются с Cached: на графике это одна величина.
func parseMeminfo(s string) (memInfo, error) {
	var out memInfo
	seen := false
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// Единица всегда kB; строки без неё (HugePages_*) нам не нужны.
		if len(fields) > 1 {
			v <<= 10
		}
		switch name {
		case "MemTotal":
			out.total, seen = v, true
		case "MemAvailable":
			out.available = v
		case "Cached", "Buffers":
			out.cached += v
		case "SwapTotal":
			out.swapTotal = v
		case "SwapFree":
			out.swapFree = v
		}
	}
	if !seen {
		return memInfo{}, errNoField
	}
	return out, nil
}

// parseLoadavg разбирает /proc/loadavg.
func parseLoadavg(s string) (l1, l5, l15 float64, err error) {
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return 0, 0, 0, errNoField
	}
	if l1, err = strconv.ParseFloat(fields[0], 64); err != nil {
		return 0, 0, 0, err
	}
	if l5, err = strconv.ParseFloat(fields[1], 64); err != nil {
		return 0, 0, 0, err
	}
	if l15, err = strconv.ParseFloat(fields[2], 64); err != nil {
		return 0, 0, 0, err
	}
	return l1, l5, l15, nil
}

// parsePidStat достаёт utime+stime из /proc/<pid>/stat.
//
// Резать строку по пробелам с начала НЕЛЬЗЯ: второе поле — имя исполняемого
// файла в скобках, и оно содержит что угодно, включая пробелы и сами скобки.
// У ffmpeg имя короткое, но проверять это на проде мы не подписывались:
// разбор идёт от ПОСЛЕДНЕЙ закрывающей скобки, как и делает сам ядерный
// формат (после неё поля фиксированы).
func parsePidStat(s string) (procTimes, error) {
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return procTimes{}, errNoField
	}
	// После имени идёт state, то есть поле 3. utime — 14-е, stime — 15-е.
	fields := strings.Fields(s[end+1:])
	if len(fields) < 13 {
		return procTimes{}, errNoField
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	return procTimes{ticks: utime + stime}, nil
}

// parseStatm достаёт resident set size из /proc/<pid>/statm (в страницах).
func parseStatm(s string, pageSize int64) (int64, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0, errNoField
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}
