package mediasource

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestStallClock(t *testing.T) {
	var s stallClock
	t0 := time.Unix(1000, 0)
	if got := s.stalledFor(t0); got != 0 {
		t.Fatalf("без чтений простой %s", got)
	}

	s.begin(t0)
	if got := s.stalledFor(t0.Add(31 * time.Second)); got != 31*time.Second {
		t.Fatalf("одно ждущее чтение: %s", got)
	}

	s.begin(t0.Add(10 * time.Second))
	s.end(t0.Add(20*time.Second), 4096)
	if got := s.stalledFor(t0.Add(31 * time.Second)); got != 11*time.Second {
		t.Errorf("данные от соседнего чтения обнуляют простой: %s", got)
	}

	s.end(t0.Add(40*time.Second), 0)
	if got := s.stalledFor(t0.Add(100 * time.Second)); got != 0 {
		t.Errorf("оборванное чтение без данных оставило простой: %s", got)
	}

	s.begin(t0.Add(200 * time.Second))
	if got := s.stalledFor(t0.Add(205 * time.Second)); got != 5*time.Second {
		t.Errorf("новое чтение после паузы унаследовало старый простой: %s", got)
	}
}

func TestReannounceScheduleBacksOff(t *testing.T) {
	r := reannounceSchedule{after: 30 * time.Second, every: 2 * time.Minute}
	t0 := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	if r.due(29*time.Second, at(0)) {
		t.Fatal("сработал раньше порога")
	}
	if !r.due(30*time.Second, at(0)) {
		t.Fatal("не сработал на пороге")
	}
	if r.due(time.Minute, at(time.Minute)) {
		t.Error("повтор раньше паузы")
	}
	if !r.due(3*time.Minute, at(2*time.Minute)) {
		t.Error("нет повтора через паузу")
	}
	if r.due(5*time.Minute, at(5*time.Minute)) {
		t.Error("вторая пауза не удвоилась")
	}
	if !r.due(7*time.Minute, at(6*time.Minute)) {
		t.Error("нет повтора через удвоенную паузу")
	}

	for range 10 {
		r.attempts++
	}
	if got := r.backoff(); got != reannounceMaxBackoff {
		t.Errorf("пауза не ограничена сверху: %s", got)
	}

	if r.due(0, at(7*time.Minute)) {
		t.Fatal("сработал без простоя")
	}
	if !r.due(30*time.Second, at(7*time.Minute+time.Second)) {
		t.Error("после восстановления новый простой ждёт старую паузу")
	}
}

func TestAnnounceErrorHidesTrackerURL(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "http://tracker.example/ann?uk=SECRETPASSKEY&peer_id=-GT0003-abc",
		Err: errors.New("dial tcp: lookup tracker.example: i/o timeout"),
	}
	got := announceError(fmt.Errorf("announce: %w", err))
	if strings.Contains(got, "SECRETPASSKEY") || strings.Contains(got, "peer_id") {
		t.Errorf("в лог уходит адрес анонса: %q", got)
	}
	if !strings.Contains(got, "i/o timeout") {
		t.Errorf("потерялась причина: %q", got)
	}

	long := announceError(errors.New(strings.Repeat("я", 1000)))
	if n := len([]rune(long)); n > announceErrorMax+1 {
		t.Errorf("ошибка не обрезана: %d символов", n)
	}
}

// Повторяет обрыв 21.11.2026: трекер уже знает живого пира, а клиент его не спрашивает.
func TestReannounceFindsPeerTrackerLearnedLater(t *testing.T) {
	sw := newStaleSwarm(t, 300*time.Millisecond)

	got, err := sw.readAll(20 * time.Second)
	if err != nil {
		t.Fatalf("чтение не дождалось данных: %v; анонсов на трекере %d", err, sw.tracker.count())
	}
	if !bytes.Equal(got, sw.data) {
		t.Fatal("прочитано не то, что раздаёт сид")
	}

	extra := sw.tracker.since(1)
	if len(extra) == 0 {
		t.Fatal("сторож не анонсировался")
	}
	q := extra[0]
	if q.Get("event") != "" {
		t.Errorf("event=%q, внеочередной анонс должен быть плановым", q.Get("event"))
	}
	if q.Get("peer_id") != sw.tracker.first().Get("peer_id") || q.Get("key") != sw.tracker.first().Get("key") {
		t.Error("внеочередной анонс представился трекеру другим клиентом")
	}
}

// Фиксирует поведение апстрима: без сторожа клиент не переспрашивает трекер до interval.
// Упадёт — anacrolix научился сам, и reannounce.go надо пересмотреть.
func TestUpstreamWaitsTrackerIntervalWithoutReannounce(t *testing.T) {
	sw := newStaleSwarm(t, 0)

	_, err := sw.readAll(3 * time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v: клиент нашёл пира сам", err)
	}
	if n := sw.tracker.count(); n != 1 {
		t.Errorf("анонсов %d, ожидался один стартовый", n)
	}
}

type staleSwarm struct {
	tracker *fakeTracker
	source  *Torrent
	data    []byte
}

func newStaleSwarm(t *testing.T, reannounceAfter time.Duration) *staleSwarm {
	t.Helper()
	tr := &fakeTracker{}
	srv := httptest.NewServer(tr)
	t.Cleanup(srv.Close)

	seedDir := t.TempDir()
	data := patternBytes('S', 512<<10)
	if err := os.WriteFile(filepath.Join(seedDir, "episode.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 32 << 10}
	if err := info.BuildFromFilePath(filepath.Join(seedDir, "episode.bin")); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	announce := srv.URL + "/announce"
	mi := &metainfo.MetaInfo{Announce: announce, AnnounceList: [][]string{{announce}}, InfoBytes: infoBytes}

	seedPort := startSeeder(t, seedDir, mi)

	torrentPath := filepath.Join(t.TempDir(), "episode.torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	leechDir := t.TempDir()
	cfg := loopbackConfig(leechDir)
	store := newStore(leechDir)
	cfg.DefaultStorage = store
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{
		client: cl, store: store, dataDir: leechDir, persistStore: true, readahead: 64 << 10,
		userAgent: cfg.HTTPUserAgent, reannounceAfter: reannounceAfter, reannounceEvery: time.Hour,
	}
	t.Cleanup(func() { c.Close() })

	src, err := c.Add(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })

	waitFor(t, "стартовый анонс клиента", func() bool { return tr.count() >= 1 && src.Ready() })
	tr.setPeer(net.IPv4(127, 0, 0, 1), seedPort)

	return &staleSwarm{tracker: tr, source: src, data: data}
}

func (s *staleSwarm) readAll(limit time.Duration) ([]byte, error) {
	r, err := s.source.Open(0)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	buf := make([]byte, len(s.data))
	_, err = io.ReadFull(WithContext(ctx, r), buf)
	return buf, err
}

func startSeeder(t *testing.T, dir string, mi *metainfo.MetaInfo) int {
	t.Helper()
	cfg := loopbackConfig(dir)
	cfg.Seed = true
	cfg.DisableTrackers = true
	store := newStore(dir)
	cfg.DefaultStorage = store
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cl.Close()
		store.Close()
	})
	tt, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	<-tt.GotInfo()
	if err := tt.VerifyDataContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "сид проверил данные", func() bool { return tt.BytesMissing() == 0 })
	return cl.LocalPort()
}

func loopbackConfig(dir string) *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.NoDHT = true
	cfg.DisableIPv6 = true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.Logger = quietLogger()
	cfg.Slogger = quietSlogger()
	return cfg
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fakeTracker struct {
	mu        sync.Mutex
	peers     []byte
	announces []url.Values
}

func (f *fakeTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.announces = append(f.announces, r.URL.Query())
	peers := string(f.peers)
	f.mu.Unlock()
	body, err := bencode.Marshal(map[string]any{"interval": 3600, "min interval": 3600, "peers": peers})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(body)
}

func (f *fakeTracker) setPeer(ip net.IP, port int) {
	compact := append([]byte{}, ip.To4()...)
	compact = binary.BigEndian.AppendUint16(compact, uint16(port))
	f.mu.Lock()
	f.peers = compact
	f.mu.Unlock()
}

func (f *fakeTracker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.announces)
}

func (f *fakeTracker) first() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.announces[0]
}

func (f *fakeTracker) since(n int) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.announces[n:]...)
}
