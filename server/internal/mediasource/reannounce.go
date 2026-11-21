package mediasource

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/tracker"
)

// anacrolix переспрашивает трекер только по его interval (у rutracker ~51 мин), сколько бы чтение ни ждало:
// после обрыва сети рой сам не оживает. Подробно — server/README.md, «Рой встал — анонс вне очереди».

const (
	reannounceNumWant    = 200
	reannounceTimeout    = 15 * time.Second
	reannounceMaxBackoff = 30 * time.Minute
)

type stallClock struct {
	mu       sync.Mutex
	pending  int
	progress time.Time
}

func (s *stallClock) begin(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == 0 {
		s.progress = now
	}
	s.pending++
}

func (s *stallClock) end(now time.Time, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if n > 0 {
		s.progress = now
	}
}

func (s *stallClock) stalledFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == 0 {
		return 0
	}
	return now.Sub(s.progress)
}

type reannounceSchedule struct {
	after    time.Duration
	every    time.Duration
	attempts int
	last     time.Time
}

func (r *reannounceSchedule) due(stalled time.Duration, now time.Time) bool {
	if stalled < r.after {
		r.attempts = 0
		return false
	}
	if r.attempts > 0 && now.Sub(r.last) < r.backoff() {
		return false
	}
	r.attempts++
	r.last = now
	return true
}

func (r *reannounceSchedule) backoff() time.Duration {
	gap := r.every
	for i := 1; i < r.attempts && gap < reannounceMaxBackoff; i++ {
		gap *= 2
	}
	return min(gap, reannounceMaxBackoff)
}

func (t *Torrent) watchStall(cl *torrent.Client, userAgent string, after, every time.Duration) {
	defer t.sampler.Done()
	sched := reannounceSchedule{after: after, every: every}
	ticker := time.NewTicker(min(after/6, 5*time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-ticker.C:
			stalled := t.stall.stalledFor(now)
			if !sched.due(stalled, now) {
				continue
			}
			tt := t.tor.Load()
			if tt == nil {
				continue
			}
			reannounce(t.ctx, cl, tt, userAgent, stalled, sched.backoff())
		}
	}
}

func reannounce(ctx context.Context, cl *torrent.Client, tt *torrent.Torrent, userAgent string, stalled, next time.Duration) {
	req := announceRequest(cl, tt)
	var (
		peers   []torrent.PeerInfo
		reports []string
	)
	mi := tt.Metainfo()
	for _, u := range mi.UpvertedAnnounceList().DistinctValues() {
		if !announceable(u) {
			continue
		}
		got, err := announceOnce(ctx, u, req, userAgent)
		if err != nil {
			reports = append(reports, fmt.Sprintf("%s: %s", trackerHost(u), announceError(err)))
			continue
		}
		reports = append(reports, fmt.Sprintf("%s: %d", trackerHost(u), len(got)))
		peers = append(peers, got...)
	}
	added := 0
	if len(peers) > 0 {
		added = tt.AddPeers(peers)
	}
	if len(reports) == 0 {
		reports = append(reports, "трекеров нет")
	}
	log.Printf("рой встал: чтение ждёт %s, пиров %d → анонс вне очереди [%s], новых адресов %d; DHT %s; следующий не раньше чем через %s",
		stalled.Round(100*time.Millisecond), tt.Stats().ActivePeers, strings.Join(reports, "; "), added,
		dhtSummary(cl), next.Round(time.Second))
}

func announceRequest(cl *torrent.Client, tt *torrent.Torrent) tracker.AnnounceRequest {
	id := cl.PeerID()
	stats := tt.Stats()
	left := int64(-1)
	if tt.Info() != nil {
		left = tt.BytesMissing()
	}
	return tracker.AnnounceRequest{
		InfoHash:   tt.InfoHash(),
		PeerId:     id,
		Downloaded: stats.BytesReadUsefulData.Int64(),
		Uploaded:   stats.BytesWrittenData.Int64(),
		Left:       left,
		Event:      tracker.None,
		// Тот же ключ, что у плановых анонсов клиента (client.go announceKey): по нему трекер узнаёт нас.
		Key:     int32(binary.BigEndian.Uint32(id[16:20])),
		NumWant: reannounceNumWant,
		Port:    uint16(cl.LocalPort()),
	}
}

func announceOnce(ctx context.Context, trackerURL string, req tracker.AnnounceRequest, userAgent string) ([]torrent.PeerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, reannounceTimeout)
	defer cancel()
	resp, err := tracker.Announce{
		TrackerUrl: trackerURL,
		Request:    req,
		UserAgent:  userAgent,
		Context:    ctx,
		Logger:     quietLogger(),
	}.Do()
	if err != nil {
		return nil, err
	}
	peers := make([]torrent.PeerInfo, 0, len(resp.Peers))
	for _, p := range resp.Peers {
		pi := torrent.PeerInfo{
			Addr:   &net.TCPAddr{IP: p.IP, Port: p.Port},
			Source: torrent.PeerSourceTracker,
		}
		copy(pi.Id[:], p.ID)
		peers = append(peers, pi)
	}
	return peers, nil
}

const announceErrorMax = 160

// В адресе анонса на приватных трекерах лежит passkey, а url.Error печатает адрес целиком.
func announceError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := []rune(err.Error())
	if len(msg) > announceErrorMax {
		return string(msg[:announceErrorMax]) + "…"
	}
	return string(msg)
}

func announceable(trackerURL string) bool {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "udp", "udp4", "udp6":
		return true
	}
	return false
}

func trackerHost(trackerURL string) string {
	if u, err := url.Parse(trackerURL); err == nil && u.Host != "" {
		return u.Host
	}
	return trackerURL
}

func dhtSummary(cl *torrent.Client) string {
	servers := cl.DhtServers()
	if len(servers) == 0 {
		return "выключен"
	}
	parts := make([]string, 0, len(servers))
	for _, s := range servers {
		parts = append(parts, fmt.Sprintf("%+v", s.Stats()))
	}
	return strings.Join(parts, " ")
}
