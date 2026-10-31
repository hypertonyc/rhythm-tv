package httpapi

import (
	"net/http"

	"github.com/avdav/torrent-media/server/internal/hls"
)

// handlePipeline отдаёт ход перекодирования: сколько видео уже произведено,
// с какой скоростью, сколько осталось до первой картинки и из чего во что
// идёт работа.
//
// Отдельный маршрут, а не поля в /api/status, потому что тот сверяется
// с Node-эталоном по форме и лишний ключ ломает сверку. Читателей два:
// телевизор во время чёрного экрана после перемотки (там нужен КОНКРЕТНЫЙ
// сеанс — свой, иначе можно показать прогресс уже заменённого) и страница
// на телефоне, которой достаточно активного. Ещё телевизор спрашивает здесь
// про запросы сегментов, прежде чем перезапустить вставший плеер.
//
// 404 без сеанса — то же, что у /api/hls-status: «сервер жив и про такой
// сеанс не знает». Телевизор на этом коде уходит в меню.
func (s *Server) handlePipeline(w http.ResponseWriter, id string) {
	p, ok := s.deps.HLS.Progress(id)
	if !ok {
		errorJSON(w, http.StatusNotFound, "HLS session not found")
		return
	}
	requests, since := s.segmentRequests(p.ID)
	writeJSON(w, http.StatusOK, struct {
		hls.Progress
		SegmentRequests       int    `json:"segmentRequests"`
		SinceSegmentRequestMs *int64 `json:"sinceSegmentRequestMs"`
	}{p, requests, since})
}

// noteSegmentRequest отмечает начало отдачи сегмента, а не её конец: Go дописывает
// сегмент в буферы ядра и nginx мгновенно, а телевизор тянет его ещё десятки секунд.
func (s *Server) noteSegmentRequest(sessionID string) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if s.deliverySession != sessionID {
		s.deliverySession = sessionID
		s.deliveryCount = 0
	}
	s.deliveryCount++
	s.deliveryAt = s.now()
}

// segmentRequests отдаёт nil вместо возраста, если этот процесс сегментов сеанса
// не отдавал: так выглядит и сеанс, подобранный после выкатки.
func (s *Server) segmentRequests(sessionID string) (int, *int64) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if sessionID == "" || s.deliverySession != sessionID {
		return 0, nil
	}
	since := s.now().Sub(s.deliveryAt).Milliseconds()
	return s.deliveryCount, &since
}
