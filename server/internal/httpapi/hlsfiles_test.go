package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/subs"
)

// observedDurations — EXTINF первых семнадцати сегментов сеанса msx85f6j
// (s03e06, режим copy) ровно как их нарезал ffmpeg на проде 17.08.2026.
// Столько было объявлено в момент, когда телевизор выбрал точку входа.
var observedDurations = []float64{
	6.131, 4.463, 3.629, 3.920, 3.837, 2.836, 7.675, 1.251, 3.378,
	4.713, 2.753, 10.636, 1.626, 1.335, 2.544, 4.088, 3.712,
}

// observedPlaylist собирает живой плейлист того сеанса: TARGETDURATION 11
// (самый длинный сегмент — 10.636), ENDLIST ещё нет.
func observedPlaylist() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:11\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for i, d := range observedDurations {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\nseg%05d.ts\n", d, i)
	}
	return b.String()
}

// firmwareJoin повторяет арифметику прошивки над плейлистом: берётся последний
// сегмент, начало которого не позже (конец плейлиста - 3*TARGETDURATION).
//
// Это не догадка о правиле, а измерение: на проде телевизор запросил первым
// seg00008, и та же формула на том же плейлисте даёт seg00008 (проверяется
// тестом ниже). Единственное допущение — что при отрицательной цели плеер
// берёт первый сегмент: другого варианта у него нет, сегментов раньше нуля
// не существует.
func firmwareJoin(playlist string) string {
	var td float64
	type seg struct {
		name  string
		start float64
	}
	var segs []seg
	var total float64
	lines := strings.Split(playlist, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			td, _ = strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64)
		case strings.HasPrefix(line, "#EXTINF:") && i+1 < len(lines):
			d, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			segs = append(segs, seg{name: lines[i+1], start: total})
			total += d
		}
	}
	if len(segs) == 0 {
		return ""
	}
	target := total - 3*td
	chosen := segs[0].name
	for _, s := range segs {
		if s.start <= target {
			chosen = s.name
		}
	}
	return chosen
}

// livePlaylist — плейлист ffmpeg таким, каким он лежит в каталоге сеанса
// на середине работы: ENDLIST ещё нет, EVENT уже есть (см. hls.BuildArgs).
const livePlaylist = "#EXTM3U\n" +
	"#EXT-X-VERSION:3\n" +
	"#EXT-X-TARGETDURATION:10\n" +
	"#EXT-X-MEDIA-SEQUENCE:0\n" +
	"#EXT-X-PLAYLIST-TYPE:EVENT\n" +
	"#EXTINF:9.051000,\nseg00000.ts\n" +
	"#EXTINF:1.209000,\nseg00001.ts\n"

func newSessionServer(t *testing.T, files map[string]string) *Server {
	t.Helper()
	return newSessionServerAge(t, files, nil)
}

// newSessionServerAge — тот же сервер, но с живым АКТИВНЫМ сеансом известного
// возраста. nil означает «активного сеанса нет».
//
// Возраст сеанса на подрезку больше не влияет — предохранитель отмеряется
// от первого запроса плейлиста, — и это проверяет
// TestJoinWindowSurvivesLateArrivingPlayer. Параметр остался затем, чтобы тому
// тесту было чем сказать «сеанс готовился 37 секунд».
func newSessionServerAge(t *testing.T, files map[string]string, age *time.Duration) *Server {
	t.Helper()
	var snap *hls.Snapshot
	if age != nil {
		snap = &hls.Snapshot{
			ID:        "abc",
			StartedAt: time.Now().Add(-*age).UnixMilli(),
			State:     "ready",
		}
	}
	return newSessionServerActive(t, files, snap)
}

// newSessionServerActive — сервер с каталогом сеанса и произвольным ответом
// на ActiveSnapshot: активным может быть и другой сеанс, не тот, чей плейлист
// спрашивают (так выглядит подобранный после выкатки).
func newSessionServerActive(t *testing.T, files map[string]string, active *hls.Snapshot) *Server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sessions := &fakeSessions{calls: new([]string), dir: dir, activeSnap: active}
	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, activeID: strings.Repeat("c", 40)}
	return New(Deps{Library: lib, HLS: sessions})
}

// freezeClock подменяет часы окна входа и отдаёт ручку, которой их двигают:
// предохранитель отмеряет секунды от первого запроса плейлиста, и ждать
// их по-настоящему незачем.
func freezeClock(s *Server) func(time.Duration) {
	now := time.Now()
	s.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

// TestPlayerPlaylistStartsAtBeginning — главный тест правки: плейлист, который
// открывает плеер, обязан явно просить начало.
//
// Без этого прошивка входит в живой плейлист за три TARGETDURATION от конца,
// а ffmpeg в режиме copy опережает реальное время в ~15 раз — телевизор
// начинал серию с 00:15-00:35, и в логе nginx первым сегментом шёл seg00008.
func TestPlayerPlaylistStartsAtBeginning(t *testing.T) {
	s := newSessionServer(t, map[string]string{hls.PlaylistName: livePlaylist})

	rec := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}

	want := "#EXTM3U\n" + startTag + "\n" + strings.TrimPrefix(livePlaylist, "#EXTM3U\n")
	if got := rec.Body.String(); got != want {
		t.Errorf("тело:\n%s\nожидалось:\n%s", got, want)
	}

	// Content-Length считается по дописанному телу, а не по размеру файла:
	// с длиной от stat телевизор получил бы обрезанный плейлист.
	if got, exp := rec.Header().Get("Content-Length"), strconv.Itoa(len(want)); got != exp {
		t.Errorf("Content-Length %q, ожидалось %q", got, exp)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type %q", ct)
	}
}

// TestFirmwareRuleReproducesTheBug — тест самого измерения. Без него следующий
// тест мог бы проходить на неверной формуле и ничего не гарантировать.
//
// На плейлисте, который прошивка видела 17.08.2026 в 12:43:11, формула обязана
// дать seg00008 — именно его телевизор и запросил первым, потеряв 33.7 с серии.
func TestFirmwareRuleReproducesTheBug(t *testing.T) {
	if got := firmwareJoin(observedPlaylist()); got != "seg00008.ts" {
		t.Fatalf("формула даёт %q, а телевизор на проде запросил seg00008.ts", got)
	}
}

// TestJoinWindowForcesFirstSegment — главный тест правки: на подрезанном
// плейлисте та же арифметика прошивки не может дать ничего, кроме seg00000.
func TestJoinWindowForcesFirstSegment(t *testing.T) {
	fresh := 2 * time.Second // столько прошло до выбора точки входа на проде
	s := newSessionServerAge(t, map[string]string{hls.PlaylistName: observedPlaylist()}, &fresh)

	rec := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	served := rec.Body.String()

	if got := firmwareJoin(served); got != "seg00000.ts" {
		t.Errorf("прошивка вошла бы в %q, а не в начало серии\nотдано:\n%s", got, served)
	}
	// Окно должно быть максимально широким: чем больше сегментов внутри
	// 3*TARGETDURATION, тем дольше плееру хватает объявленного.
	// seg00000-00006 это 32.491 с, седьмой (+1.251) вышел бы за 33.
	if n := strings.Count(served, ".ts\n"); n != 7 {
		t.Errorf("в окне %d сегментов, ожидалось 7 (32.491 с при бюджете 33)", n)
	}
	// Хвост обрезан, но не посередине пары и не в конце потока.
	if strings.Contains(served, "#EXT-X-ENDLIST") {
		t.Error("в подрезанный плейлист попал ENDLIST — плеер решит, что серия кончилась")
	}
	if !strings.HasSuffix(served, ".ts\n") {
		t.Errorf("плейлист кончается не сегментом:\n%s", served)
	}
	if !strings.Contains(served, "#EXT-X-TARGETDURATION:11\n") {
		t.Error("заголовок потерян")
	}
}

// TestJoinWindowOpensAfterPlayerTookSegment — окно держится только до выбора
// точки входа. Дальше плеер играет подряд и правило больше не применяет,
// а держать подрезку значило бы ограничить ему запас на всю серию.
func TestJoinWindowOpensAfterPlayerTookSegment(t *testing.T) {
	fresh := 2 * time.Second
	s := newSessionServerAge(t, map[string]string{
		hls.PlaylistName: observedPlaylist(),
		"seg00000.ts":    "payload",
	}, &fresh)

	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != 7 {
		t.Fatalf("до сегмента отдано %d сегментов, ожидалось 7", n)
	}
	do(s, http.MethodGet, "/hls/abc/seg00000.ts", nil)
	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != len(observedDurations) {
		t.Errorf("после сегмента отдано %d сегментов, ожидались все %d", n, len(observedDurations))
	}
}

// TestJoinWindowSurvivesLateArrivingPlayer — сеанс, который долго готовился.
//
// Телевизор ждёт в waitForHls, пока не появятся два сегмента, и к плейлисту
// приходит только после этого. 17.08.2026 в сеансе msxf9ry6 ожидание заняло
// 33 с (transcode на холодном рое сразу после выкатки), первый запрос плейлиста
// пришёл на 37-й секунде — и предохранитель, отмерявший от старта сеанса, уже
// истёк. Подрезки не было вовсе, и телевизор вошёл в seg00007, потеряв ~35 с.
//
// Возраст сеанса не имеет права ни на что влиять: считается время, которое
// плеер СМОТРИТ на плейлист, а смотреть он ещё не начинал.
func TestJoinWindowSurvivesLateArrivingPlayer(t *testing.T) {
	late := 37 * time.Second
	s := newSessionServerAge(t, map[string]string{hls.PlaylistName: observedPlaylist()}, &late)

	served := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String()
	if got := firmwareJoin(served); got != "seg00000.ts" {
		t.Errorf("прошивка вошла бы в %q, а не в начало серии\nотдано:\n%s", got, served)
	}
	if n := strings.Count(served, ".ts\n"); n != 7 {
		t.Errorf("в окне %d сегментов, ожидалось 7 (32.491 с при бюджете 33)", n)
	}
}

// TestJoinFuseCountsFromFirstLook — предохранитель. Если плеер на коротком
// плейлисте не входит вовсе, а ждёт роста, окно обязано открыться само:
// потерянное начало лучше вечного ожидания.
//
// Отсчёт идёт от первого взгляда на плейлист, поэтому проверяется парой:
// сразу подрезано, через joinFuse после первого запроса — открыто.
func TestJoinFuseCountsFromFirstLook(t *testing.T) {
	fresh := 2 * time.Second
	s := newSessionServerAge(t, map[string]string{hls.PlaylistName: observedPlaylist()}, &fresh)
	advance := freezeClock(s)

	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != 7 {
		t.Fatalf("на первом запросе отдано %d сегментов, ожидалось 7", n)
	}
	advance(joinFuse - time.Second)
	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != 7 {
		t.Errorf("до предохранителя отдано %d сегментов, ожидалось 7", n)
	}
	advance(2 * time.Second)
	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != len(observedDurations) {
		t.Errorf("после предохранителя отдано %d сегментов, ожидались все %d", n, len(observedDurations))
	}
}

// TestAdoptedSessionIsNotTruncated — подобранный после выкатки сеанс подрезать
// нельзя: его смотрят с середины, и оставить в плейлисте одно начало значило бы
// выбить у плеера то, что он играет.
//
// ENDLIST тут не спасает — его нет, если прежний ffmpeg умер посреди серии.
// Спасает то, что активным подобранный сеанс не становится (hls/adopt.go),
// а подрезается только активный. Активен здесь ДРУГОЙ сеанс: серию досматривают,
// а кто-то уже запустил новую.
func TestAdoptedSessionIsNotTruncated(t *testing.T) {
	other := &hls.Snapshot{ID: "zzz", StartedAt: time.Now().UnixMilli(), State: "ready"}
	s := newSessionServerActive(t, map[string]string{hls.PlaylistName: observedPlaylist()}, other)

	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != len(observedDurations) {
		t.Errorf("подобранный сеанс подрезан до %d сегментов из %d", n, len(observedDurations))
	}
}

// TestFinishedPlaylistIsNotTruncated — доигранный сеанс не подрезается: правило
// входа при ENDLIST не действует, а подобранный после выкатки сеанс смотрят
// с середины, и выбить у плеера список сегментов значило бы оборвать серию.
func TestFinishedPlaylistIsNotTruncated(t *testing.T) {
	fresh := 2 * time.Second
	finished := observedPlaylist() + "#EXT-X-ENDLIST\n"
	s := newSessionServerAge(t, map[string]string{hls.PlaylistName: finished}, &fresh)

	served := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String()
	if n := strings.Count(served, ".ts\n"); n != len(observedDurations) {
		t.Errorf("отдано %d сегментов, ожидались все %d", n, len(observedDurations))
	}
	if !strings.HasSuffix(served, "#EXT-X-ENDLIST\n") {
		t.Error("ENDLIST потерян — телевизор не увидит конца серии")
	}
}

// TestPlaylistWithoutSessionSnapshotIsServedWhole — активного сеанса нет,
// значит ffmpeg ничего не пишет и подрезать нечего: плейлист больше не растёт,
// и правило входа на нём — не наша забота.
func TestPlaylistWithoutSessionSnapshotIsServedWhole(t *testing.T) {
	s := newSessionServer(t, map[string]string{hls.PlaylistName: observedPlaylist()})

	if n := strings.Count(do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil).Body.String(), ".ts\n"); n != len(observedDurations) {
		t.Errorf("отдано %d сегментов, ожидались все %d", n, len(observedDurations))
	}
}

// TestHeadPlaylistLengthMatchesBody — HEAD отдаёт длину без тела, и разъехаться
// с GET она не имеет права: по ней клиент решает, сколько читать.
func TestHeadPlaylistLengthMatchesBody(t *testing.T) {
	s := newSessionServer(t, map[string]string{hls.PlaylistName: livePlaylist})

	head := do(s, http.MethodHead, "/hls/abc/"+hls.PlaylistName, nil)
	get := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil)
	if head.Body.Len() != 0 {
		t.Errorf("HEAD отдал тело: %q", head.Body.String())
	}
	if h, g := head.Header().Get("Content-Length"), get.Header().Get("Content-Length"); h != g {
		t.Errorf("Content-Length у HEAD %q, у GET %q", h, g)
	}
}

// TestOtherSessionFilesAreServedAsIs — дописывается ровно один файл.
//
// index_vtt.m3u8 разбирает наш же код, master.m3u8 не читает никто,
// а .ts вообще не текст: любая правка тела здесь была бы порчей видео.
func TestOtherSessionFilesAreServedAsIs(t *testing.T) {
	segment := "\x47\x40\x11\x10not really mpeg-ts"
	s := newSessionServer(t, map[string]string{
		subs.PlaylistName: livePlaylist,
		"master.m3u8":     livePlaylist,
		"seg00000.ts":     segment,
	})

	for name, want := range map[string]string{
		subs.PlaylistName: livePlaylist,
		"master.m3u8":     livePlaylist,
		"seg00000.ts":     segment,
	} {
		rec := do(s, http.MethodGet, "/hls/abc/"+name, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: код %d", name, rec.Code)
			continue
		}
		if got := rec.Body.String(); got != want {
			t.Errorf("%s: тело изменилось:\n%s", name, got)
		}
		if got, exp := rec.Header().Get("Content-Length"), strconv.Itoa(len(want)); got != exp {
			t.Errorf("%s: Content-Length %q, ожидалось %q", name, got, exp)
		}
	}
}

// TestStartTagIsNotDuplicated — если hlsenc когда-нибудь научится писать тег
// сам, второй экземпляр появиться не должен: два EXT-X-START в плейлисте —
// это уже неопределённое поведение.
func TestStartTagIsNotDuplicated(t *testing.T) {
	already := "#EXTM3U\n" + startTag + "\n#EXT-X-VERSION:3\n"
	s := newSessionServer(t, map[string]string{hls.PlaylistName: already})

	rec := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil)
	if got := rec.Body.String(); got != already {
		t.Errorf("тело:\n%s\nожидалось:\n%s", got, already)
	}
}

// TestPlaylistWithoutHeaderIsUntouched — портить то, чего не поняли, хуже,
// чем отдать как есть.
func TestPlaylistWithoutHeaderIsUntouched(t *testing.T) {
	garbage := "not a playlist at all\n"
	s := newSessionServer(t, map[string]string{hls.PlaylistName: garbage})

	rec := do(s, http.MethodGet, "/hls/abc/"+hls.PlaylistName, nil)
	if got := rec.Body.String(); got != garbage {
		t.Errorf("тело:\n%s", got)
	}
}
