package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/subs"
)

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
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, activeID: strings.Repeat("c", 40)}
	return New(Deps{Library: lib, HLS: &fakeSessions{calls: &calls, dir: dir}})
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
