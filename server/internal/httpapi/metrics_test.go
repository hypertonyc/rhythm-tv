package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/metrics"
)

type fakeMetrics struct {
	history []bool
}

func (f *fakeMetrics) Report(history bool) metrics.Report {
	f.history = append(f.history, history)
	out := metrics.Report{At: 1700000000000}
	if history {
		out.Series = &metrics.Series{T: []int64{1700000000000}}
	}
	return out
}

// TestMetricsHistoryOnlyOnDemand — ряды за окно наблюдения весят на два порядка
// больше самого снимка. Страница опрашивает ручку раз в две секунды и тянуть
// историю на каждом такте не должна: она забирает её при открытии экрана,
// а дальше дорисовывает по точке из обычных ответов.
func TestMetricsHistoryOnlyOnDemand(t *testing.T) {
	fake := &fakeMetrics{}
	s := New(Deps{Metrics: fake})

	if rec := do(s, http.MethodGet, "/api/metrics", nil); rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	rec := do(s, http.MethodGet, "/api/metrics?history=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if len(fake.history) != 2 || fake.history[0] || !fake.history[1] {
		t.Errorf("запрошена история %v, ожидалось [false true]", fake.history)
	}
	if !strings.Contains(rec.Body.String(), `"series":{`) {
		t.Errorf("рядов в ответе нет: %s", rec.Body.String())
	}
}

// TestMetricsWithoutCollector — сборщика может не быть (его не завели, или
// это чужая сборка), и ручка обязана сказать об этом внятным JSON: страница
// печатает поле error как есть.
func TestMetricsWithoutCollector(t *testing.T) {
	s := New(Deps{})
	rec := do(s, http.MethodGet, "/api/metrics", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error == "" {
		t.Errorf("в ответе нет объяснения: %s", rec.Body.String())
	}
}

// TestMetricsAreNotInStatus — /api/status сверяется с Node-эталоном побайтово,
// и любое поле, добавленное туда ради дашборда, ломает контракт с телевизором.
// Ровно поэтому показания живут отдельной ручкой; тест держит границу.
func TestMetricsAreNotInStatus(t *testing.T) {
	s, _, _ := newTestServer(t, nil, "")
	s.deps.Metrics = &fakeMetrics{}

	body := do(s, http.MethodGet, "/api/status", nil).Body.String()
	for _, leaked := range []string{"cpu", "memTotal", "disks", "runtime", "series"} {
		if strings.Contains(body, leaked) {
			t.Errorf("в /api/status протекло поле дашборда %q: %s", leaked, body)
		}
	}
}
