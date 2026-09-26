package hls

import "math"

const (
	// Скорость перекодирования гуляет от сцены к сцене (на проде 0.88–1.04×
	// на одной и той же серии), поэтому запас считается от заниженной скорости.
	leadSafety     = 1.05
	maxLeadSeconds = 300
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

// Выдача медленнее реального времени — телевизор ждёт запас, которого хватит
// до конца серии: при скорости r и длине T это T·(1−r).
func startupSegmentsFor(s *Session, now int64) int {
	if s.videoMode == "copy" || s.progress == nil {
		return StartupSegments
	}
	rate, ok := outputRate(s, now)
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
