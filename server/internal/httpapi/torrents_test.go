package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/library"
	"github.com/avdav/torrent-media/server/internal/mediasource"
	"github.com/avdav/torrent-media/server/internal/reclaim"
)

// fakeLibrary и fakeSessions пишут вызовы в общий журнал calls: половина
// проверок ниже — про ПОРЯДОК действий, а не про их результат.
type fakeLibrary struct {
	calls    *[]string
	current  mediasource.Source
	activeID string
	entries  []library.Entry
	uploaded []byte
	failAdd  error
}

func (f *fakeLibrary) Current() mediasource.Source { return f.current }
func (f *fakeLibrary) ActiveID() string            { return f.activeID }

func (f *fakeLibrary) List() ([]library.Entry, error) {
	out := make([]library.Entry, len(f.entries))
	copy(out, f.entries)
	for i := range out {
		out[i].Active = out[i].ID == f.activeID
	}
	return out, nil
}

func (f *fakeLibrary) Add(r io.Reader) (library.Entry, error) {
	*f.calls = append(*f.calls, "add")
	if f.failAdd != nil {
		return library.Entry{}, f.failAdd
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return library.Entry{}, err
	}
	f.uploaded = data
	entry := library.Entry{ID: strings.Repeat("a", 40), Name: "новый", File: "новый.torrent"}
	f.entries = append(f.entries, entry)
	return entry, nil
}

func (f *fakeLibrary) Activate(id string) (library.Entry, error) {
	*f.calls = append(*f.calls, "activate:"+id)
	for _, e := range f.entries {
		if e.ID == id {
			f.activeID = id
			e.Active = true
			return e, nil
		}
	}
	return library.Entry{}, library.ErrNotFound
}

func (f *fakeLibrary) Remove(id string, withData bool) error {
	if withData {
		*f.calls = append(*f.calls, "remove+data:"+id)
	} else {
		*f.calls = append(*f.calls, "remove:"+id)
	}
	kept := f.entries[:0]
	found := false
	for _, e := range f.entries {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	f.entries = kept
	if !found {
		return library.ErrNotFound
	}
	if f.activeID == id {
		f.activeID = ""
		f.current = nil
	}
	return nil
}

type fakeSessions struct {
	calls  *[]string
	active *string
	// dir — каталог сеанса, если тест проверяет отдачу файлов. Пустой означает
	// «сеанса нет», как и было до появления таких тестов.
	dir  string
	snap *hls.Snapshot
}

func (f *fakeSessions) Start(hls.StartOptions) (hls.Snapshot, error) { return hls.Snapshot{}, nil }
func (f *fakeSessions) ActiveSnapshot() *hls.Snapshot                { return nil }

// Get отдаёт снимок, только если тест его завёл: подрезка окна входа смотрит
// на StartedAt, а всем прежним тестам сеанс не нужен вовсе.
func (f *fakeSessions) Get(string) (hls.Snapshot, bool) {
	if f.snap == nil {
		return hls.Snapshot{}, false
	}
	return *f.snap, true
}

func (f *fakeSessions) SessionDir(string) (string, bool) {
	if f.dir == "" {
		return "", false
	}
	return f.dir, true
}

func (f *fakeSessions) Stop() *string {
	*f.calls = append(*f.calls, "stop")
	stopped := f.active
	f.active = nil
	return stopped
}

func newTestServer(t *testing.T, entries []library.Entry, activeID string) (*Server, *fakeLibrary, *[]string) {
	t.Helper()
	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, entries: entries, activeID: activeID}
	sessions := &fakeSessions{calls: &calls}
	return New(Deps{Library: lib, HLS: sessions}), lib, &calls
}

func do(s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestEmptyLibraryLooksLikeLoadingMetadata — весь смысл того, что активного
// торрента может не быть: телевизор об этом состоянии не знает и знать
// не должен, для него это ровно то же самое, что «метаданные грузятся».
// Тела ответов заморожены байт-в-байт вместе с остальным контрактом.
func TestEmptyLibraryLooksLikeLoadingMetadata(t *testing.T) {
	s, _, _ := newTestServer(t, nil, "")

	cases := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/api/status", http.StatusOK, `{"ready":false}`},
		{"/api/files", http.StatusServiceUnavailable, `{"error":"Torrent metadata is loading"}`},
		{"/api/probe/0", http.StatusServiceUnavailable, `{"error":"Torrent metadata is loading"}`},
		{"/api/start/0", http.StatusServiceUnavailable, `{"error":"Torrent metadata is loading"}`},
		{"/api/prebuffer/0", http.StatusServiceUnavailable, `{"error":"Torrent metadata is loading"}`},
		// /raw готовность не проверяет и не проверял: пустой список файлов
		// сам по себе даёт 404 на любом индексе.
		{"/raw/0", http.StatusNotFound, "File not found"},
	}
	for _, c := range cases {
		rec := do(s, http.MethodGet, c.path, nil)
		if rec.Code != c.wantStatus {
			t.Errorf("GET %s: код %d, ожидался %d", c.path, rec.Code, c.wantStatus)
		}
		if got := rec.Body.String(); got != c.wantBody {
			t.Errorf("GET %s: тело %q, ожидалось %q", c.path, got, c.wantBody)
		}
	}
}

// TestNotReadyTorrentStillLooksLoading — торрент выбран, но метаданные ещё
// не приехали: ответы обязаны совпадать с пустой библиотекой.
func TestNotReadyTorrentStillLooksLoading(t *testing.T) {
	fake, err := mediasource.NewFake("сериал")
	if err != nil {
		t.Fatal(err)
	}
	fake.NotReady = true

	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, current: fake, activeID: strings.Repeat("b", 40)}
	s := New(Deps{Library: lib, HLS: &fakeSessions{calls: &calls}})

	if rec := do(s, http.MethodGet, "/api/status", nil); rec.Body.String() != `{"ready":false}` {
		t.Errorf("/api/status: %q", rec.Body.String())
	}
	if rec := do(s, http.MethodGet, "/api/files", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/api/files: код %d", rec.Code)
	}
}

func TestTorrentsList(t *testing.T) {
	s, _, _ := newTestServer(t, nil, "")

	rec := do(s, http.MethodGet, "/api/torrents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	// Пустой список — [], а не null: клиент читает .length без проверки.
	// active и storage — именно null, а не отсутствующие ключи.
	if got := rec.Body.String(); got != `{"active":null,"torrents":[],"storage":null}` {
		t.Errorf("тело %q", got)
	}

	id := strings.Repeat("c", 40)
	s, _, _ = newTestServer(t, []library.Entry{{
		ID: id, Name: "Сериал", File: "Сериал.torrent", Length: 42, Files: 2, AddedAt: 1700000000000,
	}}, id)
	rec = do(s, http.MethodGet, "/api/torrents", nil)
	want := `{"active":"` + id + `","torrents":[{"id":"` + id +
		`","name":"Сериал","file":"Сериал.torrent","length":42,"files":2,"addedAt":1700000000000,"active":true}],"storage":null}`
	if got := rec.Body.String(); got != want {
		t.Errorf("тело\n  %q\nожидалось\n  %q", got, want)
	}
}

// TestUploadActivatesOnlyEmptyLibrary — загрузка с телефона не должна обрывать
// серию, которую в этот момент смотрят; но первый торрент включать некому.
func TestUploadActivatesOnlyEmptyLibrary(t *testing.T) {
	s, lib, calls := newTestServer(t, nil, "")
	rec := do(s, http.MethodPost, "/api/torrents", strings.NewReader("d4:infod4:name1:ae"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
	}
	if string(lib.uploaded) != "d4:infod4:name1:ae" {
		t.Errorf("до библиотеки доехало %q", lib.uploaded)
	}
	if len(*calls) != 2 || (*calls)[0] != "add" || !strings.HasPrefix((*calls)[1], "activate:") {
		t.Errorf("вызовы %v, ожидалось add + activate", *calls)
	}

	busy := strings.Repeat("d", 40)
	s, _, calls = newTestServer(t, []library.Entry{{ID: busy, Name: "идёт просмотр"}}, busy)
	if rec := do(s, http.MethodPost, "/api/torrents", strings.NewReader("x")); rec.Code != http.StatusCreated {
		t.Fatalf("код %d", rec.Code)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "activate:") {
			t.Errorf("загрузка переключила активный торрент: %v", *calls)
		}
	}
}

func TestUploadRejectsBadTorrent(t *testing.T) {
	s, lib, _ := newTestServer(t, nil, "")
	lib.failAdd = library.ErrBadTorrent
	if rec := do(s, http.MethodPost, "/api/torrents", strings.NewReader("мусор")); rec.Code != http.StatusBadRequest {
		t.Errorf("код %d, ожидался 400", rec.Code)
	}

	lib.failAdd = library.ErrTooLarge
	if rec := do(s, http.MethodPost, "/api/torrents", strings.NewReader("огромный")); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("код %d, ожидался 413", rec.Code)
	}

	lib.failAdd = errors.New("диск только на чтение")
	if rec := do(s, http.MethodPost, "/api/torrents", strings.NewReader("что угодно")); rec.Code != http.StatusInternalServerError {
		t.Errorf("код %d, ожидался 500", rec.Code)
	}
}

// TestUploadAcceptsMultipart — обычная HTML-форма и curl -F.
func TestUploadAcceptsMultipart(t *testing.T) {
	s, lib, _ := newTestServer(t, nil, "")

	// Граница — только ASCII: mime/multipart отвергает остальное.
	const boundary = "rtv-boundary-1"
	var body bytes.Buffer
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"torrent\"; filename=\"a.torrent\"\r\n\r\n")
	body.WriteString("содержимое")
	body.WriteString("\r\n--" + boundary + "--\r\n")

	req := httptest.NewRequest(http.MethodPost, "/api/torrents", &body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
	}
	if string(lib.uploaded) != "содержимое" {
		t.Errorf("до библиотеки доехало %q", lib.uploaded)
	}
}

// TestActivateStopsPlaybackFirst — несущий порядок: ffmpeg читает файл прежнего
// торрента петлёй через /raw, и переключение источника под живым процессом
// оставило бы его перекодировать то, чего в активном торренте уже нет.
func TestActivateStopsPlaybackFirst(t *testing.T) {
	id := strings.Repeat("e", 40)
	s, lib, calls := newTestServer(t, []library.Entry{{ID: id, Name: "другой"}}, strings.Repeat("f", 40))

	rec := do(s, http.MethodPost, "/api/torrents/"+id+"/activate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 2 || (*calls)[0] != "stop" || (*calls)[1] != "activate:"+id {
		t.Fatalf("вызовы %v, ожидалось stop + activate", *calls)
	}
	if lib.ActiveID() != id {
		t.Errorf("активен %q", lib.ActiveID())
	}
}

// TestActivateSameTorrentIsNoop — повторный тык по активному торренту не должен
// ронять просмотр.
func TestActivateSameTorrentIsNoop(t *testing.T) {
	id := strings.Repeat("e", 40)
	s, _, calls := newTestServer(t, []library.Entry{{ID: id}}, id)

	if rec := do(s, http.MethodPost, "/api/torrents/"+id+"/activate", nil); rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("вызовы %v, ожидалась тишина", *calls)
	}
}

func TestActivateUnknownTorrent(t *testing.T) {
	s, _, _ := newTestServer(t, nil, "")
	if rec := do(s, http.MethodPost, "/api/torrents/"+strings.Repeat("e", 40)+"/activate", nil); rec.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", rec.Code)
	}
}

// TestDeleteKeepsDataByDefault — данных там десятки гигабайт, и вернуть их
// можно только повторной закачкой.
func TestDeleteKeepsDataByDefault(t *testing.T) {
	id := strings.Repeat("e", 40)

	s, _, calls := newTestServer(t, []library.Entry{{ID: id}}, "")
	if rec := do(s, http.MethodPost, "/api/torrents/"+id+"/delete", nil); rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if len(*calls) != 1 || (*calls)[0] != "remove:"+id {
		t.Errorf("вызовы %v", *calls)
	}

	s, _, calls = newTestServer(t, []library.Entry{{ID: id}}, "")
	if rec := do(s, http.MethodPost, "/api/torrents/"+id+"/delete?data=1", nil); rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if len(*calls) != 1 || (*calls)[0] != "remove+data:"+id {
		t.Errorf("вызовы %v", *calls)
	}
}

// TestDeleteActiveStopsPlayback — удаляем то, что сейчас играет.
func TestDeleteActiveStopsPlayback(t *testing.T) {
	id := strings.Repeat("e", 40)
	s, _, calls := newTestServer(t, []library.Entry{{ID: id}}, id)

	if rec := do(s, http.MethodPost, "/api/torrents/"+id+"/delete", nil); rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	if len(*calls) != 2 || (*calls)[0] != "stop" || (*calls)[1] != "remove:"+id {
		t.Errorf("вызовы %v, ожидалось stop + remove", *calls)
	}
}

// TestMutationsRequirePost: GET по ссылке-«активировать» не должен ничего
// менять — по такой ссылке ходят и предзагрузчики браузера.
func TestMutationsRequirePost(t *testing.T) {
	id := strings.Repeat("e", 40)
	for _, action := range []string{"activate", "delete"} {
		s, _, calls := newTestServer(t, []library.Entry{{ID: id}}, "")
		rec := do(s, http.MethodGet, "/api/torrents/"+id+"/"+action, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: код %d, ожидался 405", action, rec.Code)
		}
		if len(*calls) != 0 {
			t.Errorf("GET %s что-то сделал: %v", action, *calls)
		}
	}

	s, _, _ := newTestServer(t, nil, "")
	if rec := do(s, http.MethodDelete, "/api/torrents", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/torrents: код %d, ожидался 405", rec.Code)
	}
}

// TestTorrentRoutesAreStrict — маршрут не должен ловить ничего, кроме infohash
// в нижнем регистре: id уходит в имена файлов и в сравнения на клиенте.
func TestTorrentRoutesAreStrict(t *testing.T) {
	for _, path := range []string{
		"/api/torrents/short/activate",
		"/api/torrents/" + strings.Repeat("E", 40) + "/activate",
		"/api/torrents/" + strings.Repeat("e", 41) + "/activate",
		"/api/torrents/" + strings.Repeat("e", 40) + "/../delete",
		"/api/torrents/" + strings.Repeat("e", 40),
		"/api/torrents/",
	} {
		s, _, calls := newTestServer(t, nil, "")
		rec := do(s, http.MethodPost, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: код %d, ожидался 404", path, rec.Code)
		}
		if len(*calls) != 0 {
			t.Errorf("POST %s что-то сделал: %v", path, *calls)
		}
	}
}

// fakeDisk — чистка места глазами обработчиков.
type fakeDisk struct {
	touched []string
	snap    reclaim.Snapshot
}

func (d *fakeDisk) Touch(rel string)           { d.touched = append(d.touched, rel) }
func (d *fakeDisk) Snapshot() reclaim.Snapshot { return d.snap }

// TestMarkWatchedUsesStorePath — отметка о просмотре кладётся под путём файла
// В ХРАНИЛИЩЕ, а не под индексом и не под именем.
//
// Индекс принадлежит торренту: после переключения активного тот же номер
// означает другую серию, и чистка выселяла бы по чужой истории. Имя без
// каталога тоже не годится — в паках, где сезон вынесен в каталог, все серии
// называются одинаково.
func TestMarkWatchedUsesStorePath(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 2)
	for _, name := range []string{"s01e01.mkv", "s01e02.mkv"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("серия"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	src, err := mediasource.NewFake("Друзья", paths...)
	if err != nil {
		t.Fatal(err)
	}

	disk := &fakeDisk{}
	calls := make([]string, 0)
	s := New(Deps{Library: &fakeLibrary{calls: &calls, current: src}, HLS: &fakeSessions{calls: &calls}, Disk: disk})

	s.markWatched(src, 1)
	if len(disk.touched) != 1 || disk.touched[0] != "Друзья/s01e02.mkv" {
		t.Fatalf("отмечено %v, ожидался путь в хранилище", disk.touched)
	}

	// Несуществующая серия отметки не оставляет.
	s.markWatched(src, 7)
	if len(disk.touched) != 1 {
		t.Errorf("отмечено %v после запроса несуществующего файла", disk.touched)
	}

	// Без чистки обработчик просто ничего не делает.
	plain := New(Deps{Library: &fakeLibrary{calls: &calls, current: src}, HLS: &fakeSessions{calls: &calls}})
	plain.markWatched(src, 0)
}

// TestTorrentsListReportsStorage — место под скачанное видно на странице
// библиотеки. Именно здесь, а не в /api/status: тот сверяется с Node-эталоном
// побайтово, и лишнее поле сломало бы сверку.
func TestTorrentsListReportsStorage(t *testing.T) {
	calls := make([]string, 0)
	at := int64(1755400000000)
	disk := &fakeDisk{snap: reclaim.Snapshot{
		Free: 19 << 30, MinFree: 10 << 30, Evicted: 3, EvictedBytes: 2 << 30, LastEvictedAt: &at,
	}}
	s := New(Deps{Library: &fakeLibrary{calls: &calls}, HLS: &fakeSessions{calls: &calls}, Disk: disk})

	rec := do(s, http.MethodGet, "/api/torrents", nil)
	want := `{"active":null,"torrents":[],"storage":{"free":20401094656,"minFree":10737418240,` +
		`"evicted":3,"evictedBytes":2147483648,"lastEvictedAt":1755400000000}}`
	if got := rec.Body.String(); got != want {
		t.Errorf("тело\n  %q\nожидалось\n  %q", got, want)
	}
}
