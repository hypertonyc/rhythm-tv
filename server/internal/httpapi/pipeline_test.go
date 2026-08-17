package httpapi

import (
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/hls"
)

func TestPipelineServesProgress(t *testing.T) {
	encoded := int64(6000)
	speed := 112.0
	s, _, _ := newTestServer(t, nil, "")
	s.deps.HLS.(*fakeSessions).progress = &hls.Progress{
		ID: "abc", Name: "s01e01.mkv", State: "buffering",
		Segments: 1, StartupSegments: 2, StartupTargetMs: 8000,
		EncodedMs: &encoded, Speed: &speed,
		Pipeline: hls.Pipeline{Video: hls.PipelineTrack{Mode: "copy", From: "h264", To: "h264"}},
	}

	for _, target := range []string{"/api/pipeline", "/api/pipeline/abc"} {
		rec := do(s, "GET", target, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: код %d", target, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`"encodedMs":6000`, `"speed":112`, `"startupTargetMs":8000`, `"mode":"copy"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: в ответе нет %s\n%s", target, want, body)
			}
		}
	}
}

// TestPipelineWithoutSessionIs404 — тот же код, что у /api/hls-status:
// «сервер жив и про такой сеанс не знает». Телевизор на нём уходит в меню,
// а не остаётся ждать сегментов, которых не будет.
func TestPipelineWithoutSessionIs404(t *testing.T) {
	s, _, _ := newTestServer(t, nil, "")
	for _, target := range []string{"/api/pipeline", "/api/pipeline/abc"} {
		if rec := do(s, "GET", target, nil); rec.Code != 404 {
			t.Errorf("%s: код %d, ожидался 404", target, rec.Code)
		}
	}
}

// TestPipelineStaysOutOfStatus — /api/status сверяется с Node-эталоном
// по форме, и любой новый ключ в нём ломает сверку. Прогресс живёт
// в отдельном маршруте именно поэтому; тест держит границу.
func TestPipelineStaysOutOfStatus(t *testing.T) {
	encoded := int64(6000)
	s, _, _ := newTestServer(t, nil, "")
	s.deps.HLS.(*fakeSessions).progress = &hls.Progress{ID: "abc", EncodedMs: &encoded}

	body := do(s, "GET", "/api/status", nil).Body.String()
	for _, forbidden := range []string{"encodedMs", "startupTargetMs", "pipeline", "etaMs"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("%q просочился в /api/status:\n%s", forbidden, body)
		}
	}
}
