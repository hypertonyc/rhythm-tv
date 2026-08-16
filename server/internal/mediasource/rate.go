package mediasource

import "sync"

// Meter — порт npm-пакета `throughput` версии 1.0.1, того самого, на котором
// в webtorrent держится torrent.downloadSpeed.
//
// Своей скользящей средней у anacrolix нет: он отдаёт накопительные счётчики.
// Считать скорость «в лоб» (дельта делить на интервал) значило бы отдавать
// телевизору куда более дёрганое число, чем он показывал раньше, поэтому
// алгоритм перенесён как есть, а не пересочинён.
//
// Устройство: кольцевой буфер накопительных сумм с шагом 100 мс. На каждом
// вызове указатель продвигается на столько тиков, сколько прошло времени,
// копируя вперёд текущую сумму, дельта добавляется в голову, а скорость —
// это разница между головой и хвостом окна.
type Meter struct {
	mu sync.Mutex

	nowMillis func() int64 // подменяется в тестах
	start     int64

	buffer  []float64
	size    int
	pointer int
	last    int
}

const (
	meterResolution = 10  // тиков в секунду
	meterTimeDiff   = 100 // мс в тике: 1000 / resolution
	meterTickMask   = 65535
)

// NewMeter создаёт измеритель с окном в seconds секунд (в webtorrent — 5).
func NewMeter(seconds int, nowMillis func() int64) *Meter {
	if seconds <= 0 {
		seconds = 5
	}
	m := &Meter{
		nowMillis: nowMillis,
		start:     nowMillis(),
		size:      meterResolution * seconds,
		// В оригинале buffer стартует как [0] и дорастает до size по мере
		// продвижения указателя. Длина буфера участвует в формуле, поэтому
		// заранее выделить size элементов нельзя — ответ будет другим.
		buffer:  []float64{0},
		pointer: 1,
	}
	m.last = (m.tick() - 1) & meterTickMask
	return m
}

func (m *Meter) tick() int {
	return int((m.nowMillis()-m.start)/meterTimeDiff) & meterTickMask
}

// Add скармливает дельту байт и возвращает текущую скорость в байтах в секунду.
// Add(0) — это чтение без обновления, как `this._downloadSpeed()` в webtorrent.
func (m *Meter) Add(delta float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	tick := m.tick()
	dist := (tick - m.last) & meterTickMask
	if dist > m.size {
		dist = m.size
	}
	m.last = tick

	for ; dist > 0; dist-- {
		if m.pointer == m.size {
			m.pointer = 0
		}
		prev := m.pointer - 1
		if m.pointer == 0 {
			prev = m.size - 1
		}
		m.set(m.pointer, m.get(prev))
		m.pointer++
	}

	if delta != 0 {
		m.set(m.pointer-1, m.get(m.pointer-1)+delta)
	}

	top := m.get(m.pointer - 1)
	var btm float64
	if len(m.buffer) >= m.size {
		idx := m.pointer
		if m.pointer == m.size {
			idx = 0
		}
		btm = m.get(idx)
	}

	if len(m.buffer) < meterResolution {
		return top
	}
	return (top - btm) * meterResolution / float64(len(m.buffer))
}

// Rate — текущая скорость без обновления.
func (m *Meter) Rate() float64 { return m.Add(0) }

// get и set повторяют поведение JS-массива: запись за границу его удлиняет,
// чтение за границей даёт «пусто». Именно на растущей длине буфера построена
// формула, поэтому подменять её фиксированным слайсом нельзя.
func (m *Meter) get(i int) float64 {
	if i < 0 || i >= len(m.buffer) {
		return 0
	}
	return m.buffer[i]
}

func (m *Meter) set(i int, v float64) {
	if i < 0 {
		return
	}
	for len(m.buffer) <= i {
		m.buffer = append(m.buffer, 0)
	}
	m.buffer[i] = v
}
