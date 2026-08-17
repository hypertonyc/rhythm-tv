package httpapi

import (
	"net/http"

	"github.com/avdav/torrent-media/server/internal/metrics"
)

// Ручка дашборда.
//
// Отдельная от /api/status намеренно и без вариантов: тот сверяется
// с Node-эталоном побайтово, и любое лишнее поле в нём ломает сверку
// контракта с телевизором. Здесь же эталона нет вовсе — как и у /api/torrents, —
// поэтому формат можно менять свободно, а телевизор сюда не ходит.

// handleMetrics: GET /api/metrics[?history=1].
//
// История отдаётся ТОЛЬКО по запросу: это десяток килобайт рядов, и тянуть
// их каждые две секунды незачем — страница дорисовывает свежую точку из того
// же ответа сама, а полные ряды забирает при открытии экрана.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.deps.Metrics == nil {
		errorJSON(w, http.StatusServiceUnavailable, "Metrics are not collected")
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Metrics.Report(r.URL.Query().Get("history") == "1"))
}

// Metrics — сборщик показаний глазами HTTP-слоя; это *metrics.Collector.
//
// Интерфейс, а не структура, по той же причине, что и у остальных зависимостей:
// обработчик должен проверяться без /proc, statfs и живого ffmpeg.
type Metrics interface {
	Report(history bool) metrics.Report
}
