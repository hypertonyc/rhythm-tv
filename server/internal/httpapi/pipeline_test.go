package httpapi

import (
	"strings"
	"testing"
	"time"

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

// TestPipelineReportsSegmentRequests — по возрасту последнего запроса сегмента
// телевизор отличает медленную сеть от вставшего плеера, прежде чем перезапускать.
func TestPipelineReportsSegmentRequests(t *testing.T) {
	s := newSessionServerActive(t, map[string]string{"seg00000.ts": "x", "seg00001.ts": "y"}, nil)
	s.deps.HLS.(*fakeSessions).progress = &hls.Progress{ID: "abc"}
	advance := freezeClock(s)

	if body := do(s, "GET", "/api/pipeline/abc", nil).Body.String(); !strings.Contains(body, `"segmentRequests":0,"sinceSegmentRequestMs":null`) {
		t.Fatalf("до первого сегмента возраст обязан быть null:\n%s", body)
	}

	do(s, "GET", "/hls/abc/seg00000.ts", nil)
	advance(20 * time.Second)
	do(s, "GET", "/hls/abc/seg00001.ts", nil)
	advance(7 * time.Second)

	body := do(s, "GET", "/api/pipeline/abc", nil).Body.String()
	if !strings.Contains(body, `"segmentRequests":2,"sinceSegmentRequestMs":7000`) {
		t.Fatalf("ждали два запроса и 7 с с последнего:\n%s", body)
	}
}

// TestPipelineSegmentRequestsBelongToTheirSession — запросы прежнего сеанса
// не должны выдавать новый за живой: после перемотки это уже другой плеер.
func TestPipelineSegmentRequestsBelongToTheirSession(t *testing.T) {
	s := newSessionServerActive(t, map[string]string{"seg00000.ts": "x"}, nil)
	freezeClock(s)
	do(s, "GET", "/hls/abc/seg00000.ts", nil)

	s.deps.HLS.(*fakeSessions).progress = &hls.Progress{ID: "xyz"}
	if body := do(s, "GET", "/api/pipeline/xyz", nil).Body.String(); !strings.Contains(body, `"segmentRequests":0,"sinceSegmentRequestMs":null`) {
		t.Fatalf("чужие запросы попали в сеанс xyz:\n%s", body)
	}
}

// TestPlaylistIsNotASegmentRequest — плейлист плеер перечитывает и встав,
// поэтому запросом сегмента он не считается.
func TestPlaylistIsNotASegmentRequest(t *testing.T) {
	s := newSessionServerActive(t, map[string]string{hls.PlaylistName: "#EXTM3U\n", "index0.vtt": "WEBVTT\n"}, nil)
	s.deps.HLS.(*fakeSessions).progress = &hls.Progress{ID: "abc"}
	do(s, "GET", "/hls/abc/"+hls.PlaylistName, nil)
	do(s, "GET", "/hls/abc/index0.vtt", nil)
	if body := do(s, "GET", "/api/pipeline/abc", nil).Body.String(); !strings.Contains(body, `"sinceSegmentRequestMs":null`) {
		t.Fatalf("плейлист или субтитры засчитаны как сегмент:\n%s", body)
	}
}
