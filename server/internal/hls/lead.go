package hls

import (
	"encoding/json"
	"log"
	"math"
	"os"
)

const (
	// Скорость перекодирования гуляет от сцены к сцене (на проде 0.88–1.04×
	// на одной и той же серии), поэтому запас считается от заниженной скорости.
	leadSafety     = 1.05
	maxLeadSeconds = 300
	// Короче минуты материала скорость меряет начало серии, а оно обычно самое лёгкое.
	minRateSampleSeconds = 60
)

// outputRate — секунд видео за секунду времени, считая от первого готового сегмента:
// декодирование с начала серии, которое делает output-side -ss, сюда не попадает.
func outputRate(s *Session, now int64) (float64, bool) {
	intervals := s.segments - s.firstOutputSegs
	if s.firstOutputAt == 0 || intervals < 2 || now <= s.firstOutputAt {
		return 0, false
	}
	return float64(intervals*SegmentSeconds) / (float64(now-s.firstOutputAt) / 1000), true
}

// startupRate — скорость, от которой считается запас: худшая из замеренной сейчас
// и обычной для этого формата. usual == 0 — формат ещё не встречался.
func startupRate(s *Session, now int64, usual float64) (float64, bool) {
	rate, ok := outputRate(s, now)
	switch {
	case ok && usual > 0:
		return math.Min(rate, usual), true
	case usual > 0:
		return usual, true
	}
	return rate, ok
}

// Выдача медленнее реального времени — телевизор ждёт запас, которого хватит
// до конца серии: при скорости r и длине T это T·(1−r).
func startupSegmentsFor(s *Session, now int64, usual float64) int {
	if s.videoMode == "copy" || s.progress == nil {
		return StartupSegments
	}
	rate, ok := startupRate(s, now, usual)
	if !ok {
		// Без замера не отпускаем: иначе медленный сеанс стартовал бы на двух
		// сегментах раньше, чем стало видно, что он медленный.
		return StartupSegments + 1
	}
	lead := math.Min((s.duration-s.start)*(1-rate/leadSafety), maxLeadSeconds)
	if need := int(math.Ceil(lead / SegmentSeconds)); need > StartupSegments {
		return need
	}
	return StartupSegments
}

// rateKey — формат перекодирования: серии одного релиза делят его целиком,
// и скорость прошлой серии предсказывает следующую лучше её собственного начала.
func rateKey(s *Session) string {
	return s.pipeline.Video.From + " → " + s.pipeline.Video.To
}

func (m *Manager) usualRateLocked(s *Session) float64 {
	m.loadRatesLocked()
	return m.rates[rateKey(s)]
}

// Зовётся, когда ffmpeg вышел, и до перезаписи lastOutputAt: скорость меряется
// от первого сегмента до последнего, а не до выхода процесса.
func (m *Manager) rememberRateLocked(s *Session) {
	if s.videoMode == "copy" || s.lastOutputAt == nil || s.firstOutputAt == 0 {
		return
	}
	produced := (s.segments - s.firstOutputSegs) * SegmentSeconds
	elapsed := *s.lastOutputAt - s.firstOutputAt
	if produced < minRateSampleSeconds || elapsed <= 0 {
		return
	}
	sample := float64(produced) / (float64(elapsed) / 1000)

	m.loadRatesLocked()
	key := rateKey(s)
	if old, ok := m.rates[key]; ok {
		sample = (old + sample) / 2
	}
	m.rates[key] = sample
	m.saveRatesLocked()
}

func (m *Manager) loadRatesLocked() {
	if m.rates != nil {
		return
	}
	m.rates = make(map[string]float64)
	if m.RatesPath == "" {
		return
	}
	raw, err := os.ReadFile(m.RatesPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("HLS скорости перекодирования %s: %v", m.RatesPath, err)
		}
		return
	}
	if err := json.Unmarshal(raw, &m.rates); err != nil {
		log.Printf("HLS скорости перекодирования %s: %v", m.RatesPath, err)
		m.rates = make(map[string]float64)
	}
}

func (m *Manager) saveRatesLocked() {
	if m.RatesPath == "" {
		return
	}
	raw, err := json.MarshalIndent(m.rates, "", "  ")
	if err != nil {
		return
	}
	tmp := m.RatesPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("HLS скорости перекодирования %s: %v", m.RatesPath, err)
		return
	}
	if err := os.Rename(tmp, m.RatesPath); err != nil {
		log.Printf("HLS скорости перекодирования %s: %v", m.RatesPath, err)
	}
}
