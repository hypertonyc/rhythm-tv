package reclaim

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avdav/torrent-media/server/internal/library"
)

// Тесты чистки. Все — про ПОРЯДОК и ОСТАНОВКУ: удалять данные легко, а вот
// удалить не то или не остановиться вовремя означает выбросить десятки
// гигабайт, которые вернутся только повторной закачкой.

// fakeStore удаляет файл и отдаёт, сколько он занимал, — как настоящий
// mediasource.Client, только без снятия отметок готовности.
type fakeStore struct {
	storeDir string
	byKey    map[string]string // «.torrent#индекс» → путь относительно хранилища
	dropped  []string
	// freed растит свободное место, как это делает настоящее удаление.
	freed *int64
	fail  map[string]error
}

func (s *fakeStore) DropFile(torrentPath string, index int) (int64, error) {
	rel := s.byKey[torrentPath+"#"+strconv.Itoa(index)]
	if err := s.fail[rel]; err != nil {
		return 0, err
	}
	full := filepath.Join(s.storeDir, filepath.FromSlash(rel))
	n, _, err := fileUsage(full)
	if err != nil {
		return 0, err
	}
	if err := os.Remove(full); err != nil {
		return 0, err
	}
	s.dropped = append(s.dropped, rel)
	*s.freed += n
	return n, nil
}

type fakeCatalog struct {
	files []library.StoredFile
	err   error
}

func (c *fakeCatalog) StoredFiles() ([]library.StoredFile, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.files, nil
}

// fixture — хранилище на диске, библиотека над ним и чистка.
type fixture struct {
	t     *testing.T
	dir   string
	cat   *fakeCatalog
	store *fakeStore
	free  int64
	now   time.Time
	k     *Keeper
}

func newFixture(t *testing.T, minFree, target, free int64) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		t:     t,
		dir:   dir,
		cat:   &fakeCatalog{},
		free:  free,
		now:   time.Date(2026, 8, 17, 21, 0, 0, 0, time.UTC),
		store: &fakeStore{storeDir: dir, byKey: map[string]string{}, fail: map[string]error{}},
	}
	f.store.freed = &f.free
	f.k = New(Options{
		StoreDir:   dir,
		MinFree:    minFree,
		TargetFree: target,
		// Иначе сюда не пройдёт ни один файл: тестовые весят килобайты,
		// а порог по умолчанию — 16 МБ.
		MinFileBytes: 1,
		JournalPath:  filepath.Join(t.TempDir(), ".tms-watched"),
		Catalog:      f.cat,
		Store:        f.store,
		Now:          func() time.Time { return f.now },
		Free:         func(string) (int64, error) { return f.free, nil },
	})
	return f
}

// add кладёт файл в хранилище и заводит на него запись библиотеки.
// watchedAgo < 0 означает «не смотрели»; тогда порядок решает mtime.
// Торрент — первый сегмент пути, куски у каждого файла свои, пока их
// не переставит pieces.
func (f *fixture) add(rel string, size int, watchedAgo, modifiedAgo time.Duration) {
	f.t.Helper()
	full := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	mod := f.now.Add(-modifiedAgo)
	if err := os.Chtimes(full, mod, mod); err != nil {
		f.t.Fatal(err)
	}

	torrentID, _, _ := strings.Cut(rel, "/")
	torrentFile := filepath.Join(f.dir, "..", torrentID+".torrent")
	index := len(f.cat.files)
	f.cat.files = append(f.cat.files, library.StoredFile{
		TorrentID:   torrentID,
		TorrentFile: torrentFile,
		Index:       index,
		Rel:         rel,
		Length:      int64(size),
		FirstPiece:  index * 10,
		LastPiece:   index*10 + 9,
	})
	f.store.byKey[torrentFile+"#"+strconv.Itoa(index)] = rel
	if watchedAgo >= 0 {
		f.k.journal.Touch(rel, f.now.Add(-watchedAgo))
	}
}

func (f *fixture) pieces(rel string, first, last int) {
	f.t.Helper()
	for i := range f.cat.files {
		if f.cat.files[i].Rel == rel {
			f.cat.files[i].FirstPiece, f.cat.files[i].LastPiece = first, last
			return
		}
	}
	f.t.Fatalf("файла %q в библиотеке нет", rel)
}

func (f *fixture) protect(rels ...string) {
	f.k.opts.Protected = func() []string { return rels }
}

func (f *fixture) expectDropped(want ...string) {
	f.t.Helper()
	if len(f.store.dropped) != len(want) {
		f.t.Fatalf("выселено %v, ожидалось %v", f.store.dropped, want)
	}
	for i := range want {
		if f.store.dropped[i] != want[i] {
			f.t.Fatalf("выселено %v, ожидалось %v", f.store.dropped, want)
		}
	}
}

func (f *fixture) expectKept(rels ...string) {
	f.t.Helper()
	for _, rel := range rels {
		if !exists(f.t, filepath.Join(f.dir, filepath.FromSlash(rel))) {
			f.t.Errorf("выселен %q, а его трогать было нельзя", rel)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestSweepEvictsLeastRecentlyWatchedFirst — то, ради чего всё написано.
func TestSweepEvictsLeastRecentlyWatchedFirst(t *testing.T) {
	// Цель заведомо недостижима: так проверяется весь порядок, а не первый шаг.
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)
	f.add("Друзья/s01e02.mkv", 8<<10, time.Hour, time.Hour)
	f.add("tbbt/S05E10.mkv", 8<<10, 7*24*time.Hour, time.Hour)

	f.k.Sweep()

	want := []string{"Друзья/s01e01.mkv", "tbbt/S05E10.mkv", "Друзья/s01e02.mkv"}
	if len(f.store.dropped) != len(want) {
		t.Fatalf("выселено %v, ожидалось %v", f.store.dropped, want)
	}
	for i := range want {
		if f.store.dropped[i] != want[i] {
			t.Errorf("на месте %d выселено %q, ожидалось %q", i, f.store.dropped[i], want[i])
		}
	}
}

// TestSweepStopsAtTarget — чистка останавливается, добравшись до цели, а не
// выгребает библиотеку целиком.
func TestSweepStopsAtTarget(t *testing.T) {
	// Свободно 0, надо 1 байт: хватит первого же выселенного файла.
	f := newFixture(t, 100, 1, 0)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)
	f.add("Друзья/s01e02.mkv", 8<<10, 20*24*time.Hour, time.Hour)
	f.add("Друзья/s01e03.mkv", 8<<10, 10*24*time.Hour, time.Hour)

	if freed := f.k.Sweep(); freed <= 0 {
		t.Fatalf("освобождено %d байт", freed)
	}
	if len(f.store.dropped) != 1 || f.store.dropped[0] != "Друзья/s01e01.mkv" {
		t.Fatalf("выселено %v, ожидалась одна самая старая серия", f.store.dropped)
	}
	if !exists(t, filepath.Join(f.dir, "Друзья", "s01e02.mkv")) {
		t.Error("выселена серия, до которой очередь не должна была дойти")
	}
}

// TestSweepDoesNothingAboveThreshold — пока места хватает, библиотека
// не трогается вовсе.
func TestSweepDoesNothingAboveThreshold(t *testing.T) {
	f := newFixture(t, 100, 200, 100)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)

	if freed := f.k.Sweep(); freed != 0 {
		t.Errorf("освобождено %d байт, а место было в порядке", freed)
	}
	if len(f.store.dropped) != 0 {
		t.Errorf("выселено %v при достаточном месте", f.store.dropped)
	}
}

// TestSweepKeepsProtectedFiles — то, что отдал Protected, не выселяется, даже
// если по журналу оно самое старое: вынутые куски обернулись бы паузой.
func TestSweepKeepsProtectedFiles(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)
	f.add("Друзья/s01e02.mkv", 8<<10, 40*24*time.Hour, time.Hour)
	f.add("Друзья/s01e03.mkv", 8<<10, 20*24*time.Hour, time.Hour)
	f.protect("Друзья/s01e01.mkv", "Друзья/s01e02.mkv")

	f.k.Sweep()

	f.expectDropped("Друзья/s01e03.mkv")
	f.expectKept("Друзья/s01e01.mkv", "Друзья/s01e02.mkv")
}

// TestSweepSkipsNeighbourSharingPieceWithProtected — DropFile снял бы отметку
// и с граничного куска, а с ним пропали бы начало или хвост защищённого соседа.
func TestSweepSkipsNeighbourSharingPieceWithProtected(t *testing.T) {
	for _, c := range []struct {
		name      string
		protected string
		neighbour string
		first     int
		last      int
		protFirst int
		protLast  int
	}{
		{name: "хвост соседа — начало играющей", protected: "Друзья/s02e03.mkv",
			neighbour: "Друзья/s03e14.mkv", first: 50, last: 100, protFirst: 100, protLast: 200},
		{name: "начало соседа — хвост играющей", protected: "Друзья/s02e03.mkv",
			neighbour: "Друзья/s03e14.mkv", first: 200, last: 260, protFirst: 100, protLast: 200},
		{name: "сосед целиком внутри куска защищённого", protected: "Друзья/s02e03.mkv",
			neighbour: "Друзья/s03e14.mkv", first: 150, last: 150, protFirst: 100, protLast: 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 1<<40, 1<<40, 0)
			f.add(c.protected, 8<<10, time.Hour, time.Hour)
			f.add(c.neighbour, 8<<10, 400*24*time.Hour, time.Hour)
			f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)
			f.pieces(c.protected, c.protFirst, c.protLast)
			f.pieces(c.neighbour, c.first, c.last)
			f.pieces("Друзья/s01e01.mkv", 0, 49)
			f.protect(c.protected)

			f.k.Sweep()

			f.expectDropped("Друзья/s01e01.mkv")
			f.expectKept(c.protected, c.neighbour)
		})
	}
}

// TestSweepReplaysIncident20261125 — доиграла s02e03, начало следующей s02e04 делило кусок
// 2935 с s03e14, самой старой по журналу; её выселение раз в минуту сорвало автопереход.
func TestSweepReplaysIncident20261125(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s02e03.mkv", 8<<10, 0, time.Hour)
	f.add("Друзья/s02e04.mkv", 8<<10, -1, 2*time.Minute)
	f.add("Друзья/s03e14.mkv", 8<<10, 60*24*time.Hour, time.Hour)
	f.add("Друзья/s01e05.mkv", 8<<10, 50*24*time.Hour, time.Hour)
	f.add("Друзья/s01e06.mkv", 8<<10, 10*24*time.Hour, time.Hour)
	f.pieces("Друзья/s02e03.mkv", 2700, 2850)
	f.pieces("Друзья/s02e04.mkv", 2935, 3080)
	f.pieces("Друзья/s03e14.mkv", 2860, 2935)
	f.pieces("Друзья/s01e05.mkv", 100, 250)
	f.pieces("Друзья/s01e06.mkv", 250, 400)
	f.protect("Друзья/s02e03.mkv", "Друзья/s02e04.mkv")

	f.k.Sweep()

	f.expectDropped("Друзья/s01e05.mkv", "Друзья/s01e06.mkv")
	f.expectKept("Друзья/s02e03.mkv", "Друзья/s02e04.mkv", "Друзья/s03e14.mkv")
}

// TestSharedPieceCountsOnlyWithinTorrent — номера кусков у каждого торрента
// свои, и кусок 2935 другого сериала с защищённой серией ничего не делит.
func TestSharedPieceCountsOnlyWithinTorrent(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s02e04.mkv", 8<<10, -1, 2*time.Minute)
	f.add("tbbt/S05E11.mkv", 8<<10, 60*24*time.Hour, time.Hour)
	f.pieces("Друзья/s02e04.mkv", 2935, 3080)
	f.pieces("tbbt/S05E11.mkv", 2860, 2935)
	f.protect("Друзья/s02e04.mkv")

	f.k.Sweep()

	f.expectDropped("tbbt/S05E11.mkv")
}

// TestSweepEvictsFileOnceReaderCloses — открытый читатель защищает файл ровно на время
// чтения: Protected спрашивается на каждом проходе.
func TestSweepEvictsFileOnceReaderCloses(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s02e04.mkv", 8<<10, 400*24*time.Hour, time.Hour)
	reading := true
	f.k.opts.Protected = func() []string {
		if reading {
			return []string{"Друзья/s02e04.mkv"}
		}
		return nil
	}

	f.k.Sweep()
	f.expectDropped()

	reading = false
	f.k.Sweep()
	f.expectDropped("Друзья/s02e04.mkv")
}

// TestSweepFallsBackToModTime — у файла, который ещё не смотрели, порядок
// решает время файла. Свежескачанная серия не должна вылетать раньше той,
// что смотрели год назад.
func TestSweepFallsBackToModTime(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("новое/s02e01.mkv", 8<<10, -1, time.Hour)          // не смотрели, скачано час назад
	f.add("старое/s01e01.mkv", 8<<10, 365*24*time.Hour, 0)   // смотрели год назад
	f.add("забытое/s00e01.mkv", 8<<10, -1, 400*24*time.Hour) // не смотрели, лежит больше года

	f.k.Sweep()

	want := []string{"забытое/s00e01.mkv", "старое/s01e01.mkv", "новое/s02e01.mkv"}
	for i := range want {
		if i >= len(f.store.dropped) || f.store.dropped[i] != want[i] {
			t.Fatalf("выселено %v, ожидалось %v", f.store.dropped, want)
		}
	}
}

// TestSweepSkipsSmallFiles — паки субтитров и .nfo выселять бессмысленно:
// места они не освободят, а запись о просмотре потеряется.
func TestSweepSkipsSmallFiles(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.k.opts.MinFileBytes = 1 << 20
	f.add("Друзья/s01e01.srt", 100, 30*24*time.Hour, time.Hour)
	f.add("Друзья/s01e01.mkv", 4<<20, 10*24*time.Hour, time.Hour)

	f.k.Sweep()

	if len(f.store.dropped) != 1 || f.store.dropped[0] != "Друзья/s01e01.mkv" {
		t.Fatalf("выселено %v, ожидалась только серия", f.store.dropped)
	}
}

// TestSweepSurvivesFailedEviction — отказ на одном файле не должен обрывать
// проход: место кончается прямо сейчас, а причина отказа бывает частной.
func TestSweepSurvivesFailedEviction(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)
	f.add("Друзья/s01e02.mkv", 8<<10, 20*24*time.Hour, time.Hour)
	f.store.fail["Друзья/s01e01.mkv"] = os.ErrPermission

	f.k.Sweep()

	if len(f.store.dropped) != 1 || f.store.dropped[0] != "Друзья/s01e02.mkv" {
		t.Fatalf("выселено %v, ожидалась вторая серия после отказа на первой", f.store.dropped)
	}
}

// TestDisabledKeeperStillReportsFreeSpace — с MinFree=0 чистка не удаляет
// ничего, но место на странице библиотеки всё равно показывается.
func TestDisabledKeeperStillReportsFreeSpace(t *testing.T) {
	f := newFixture(t, 0, 0, 42)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)

	if f.k.Enabled() {
		t.Error("чистка с нулевым порогом считает себя включённой")
	}
	if freed := f.k.Sweep(); freed != 0 || len(f.store.dropped) != 0 {
		t.Errorf("выключенная чистка выселила %v", f.store.dropped)
	}
	snap := f.k.Snapshot()
	if snap.Free != 42 || snap.MinFree != 0 {
		t.Errorf("снимок %+v, ожидалось free=42 minFree=0", snap)
	}
}

// TestSnapshotCountsEvictions — страница библиотеки показывает итог чистки.
func TestSnapshotCountsEvictions(t *testing.T) {
	f := newFixture(t, 1<<40, 1<<40, 0)
	f.add("Друзья/s01e01.mkv", 8<<10, 30*24*time.Hour, time.Hour)

	if snap := f.k.Snapshot(); snap.LastEvictedAt != nil {
		t.Error("до первого выселения время последнего обязано быть null")
	}
	freed := f.k.Sweep()

	snap := f.k.Snapshot()
	if snap.Evicted != 1 || snap.EvictedBytes != freed {
		t.Errorf("снимок %+v, ожидалось 1 файл на %d байт", snap, freed)
	}
	if snap.LastEvictedAt == nil || *snap.LastEvictedAt != f.now.UnixMilli() {
		t.Errorf("время последнего выселения %v", snap.LastEvictedAt)
	}
}

// TestSnapshotReportsUnknownFreeSpace — «не смогли измерить» отличается
// от «места нет»: ноль здесь означал бы забитый диск.
func TestSnapshotReportsUnknownFreeSpace(t *testing.T) {
	k := New(Options{StoreDir: t.TempDir(), MinFree: 1 << 30,
		Free: func(string) (int64, error) { return 0, os.ErrNotExist }})
	if got := k.Snapshot().Free; got != -1 {
		t.Errorf("Free = %d, ожидалось -1", got)
	}
}

// TestTargetBelowMinIsRaised — цель ниже порога означала бы чистку на каждом
// тике по одному файлу.
func TestTargetBelowMinIsRaised(t *testing.T) {
	k := New(Options{MinFree: 10, TargetFree: 5})
	if k.opts.TargetFree != k.opts.MinFree {
		t.Errorf("цель %d при пороге %d", k.opts.TargetFree, k.opts.MinFree)
	}
}
