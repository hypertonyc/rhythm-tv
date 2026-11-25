package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/library"
	"github.com/avdav/torrent-media/server/internal/mediasource"
	"github.com/avdav/torrent-media/server/internal/reclaim"
)

// friendsEpisodes — порядок в метаинформации нарочно не порядок серий:
// за s02e03 (индекс 2) идёт s02e04 (индекс 0), а не s01e01 (индекс 3).
var friendsEpisodes = []string{"s02e04.mkv", "s03e14.mkv", "s02e03.mkv", "s01e01.mkv"}

func friendsSource(t *testing.T, storeDir string) *mediasource.Fake {
	t.Helper()
	dir := filepath.Join(storeDir, "Друзья")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(friendsEpisodes))
	for _, name := range friendsEpisodes {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 8<<10), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	src, err := mediasource.NewFake("Друзья", paths...)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func sameFiles(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: защищены %q, ожидалось %q", what, got, want)
	}
}

// TestProtectedFilesFollowEpisodeOrder — следующая серия та же, что у автоперехода:
// своя сортировка разъехалась бы с ним, и чистка берегла бы не ту серию.
func TestProtectedFilesFollowEpisodeOrder(t *testing.T) {
	src := friendsSource(t, t.TempDir())

	sameFiles(t, "играет s02e03", protectedFiles(src, &hls.Snapshot{Index: 2}),
		"Друзья/s02e03.mkv", "Друзья/s02e04.mkv")
	sameFiles(t, "играет последняя серия", protectedFiles(src, &hls.Snapshot{Index: 1}),
		"Друзья/s03e14.mkv")
	sameFiles(t, "сеанса нет", protectedFiles(src, nil))
	sameFiles(t, "торрента нет", protectedFiles(nil, &hls.Snapshot{Index: 2}))
}

// TestProtectedFilesIncludeOpenReaders — префетч, ffprobe и ffmpeg держат файл
// открытым через /raw; пока Reader жив, файл защищён, закрыт — защита снята.
func TestProtectedFilesIncludeOpenReaders(t *testing.T) {
	src := friendsSource(t, t.TempDir())

	prebuffer, err := src.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	sameFiles(t, "префетч без сеанса", protectedFiles(src, nil), "Друзья/s02e04.mkv")

	probe, err := src.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	sameFiles(t, "играет s01e01, читаются s02e04 и s03e14", protectedFiles(src, &hls.Snapshot{Index: 3}),
		"Друзья/s01e01.mkv", "Друзья/s02e03.mkv", "Друзья/s02e04.mkv", "Друзья/s03e14.mkv")

	prebuffer.Close()
	probe.Close()
	sameFiles(t, "читатели закрыты", protectedFiles(src, nil))
}

type sourceCatalog struct{ src *mediasource.Fake }

func (c sourceCatalog) StoredFiles() ([]library.StoredFile, error) {
	files := c.src.Files()
	out := make([]library.StoredFile, 0, len(files))
	for _, f := range files {
		out = append(out, library.StoredFile{
			TorrentID: "Друзья", TorrentFile: "Друзья.torrent", Index: f.Index, Rel: f.StorePath,
			Length: f.Length, FirstPiece: f.Index * 10, LastPiece: f.Index*10 + 9, Active: true,
		})
	}
	return out, nil
}

type removingStore struct {
	dir     string
	src     *mediasource.Fake
	dropped []string
}

func (s *removingStore) DropFile(_ string, index int) (int64, error) {
	rel := s.src.Files()[index].StorePath
	if err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(rel))); err != nil {
		return 0, err
	}
	s.dropped = append(s.dropped, rel)
	return 8 << 10, nil
}

// TestSweepKeepsNextEpisode — проводка целиком: следующая серия не выселяется, хотя
// по журналу она самая старая (25.11.2026 из-за этого упал автопереход).
func TestSweepKeepsNextEpisode(t *testing.T) {
	store := t.TempDir()
	src := friendsSource(t, store)
	session := &hls.Snapshot{Index: 2}
	now := time.Date(2026, 11, 25, 13, 43, 0, 0, time.UTC)
	dropper := &removingStore{dir: store, src: src}
	keeper := reclaim.New(reclaim.Options{
		StoreDir: store, MinFree: 1 << 40, TargetFree: 1 << 40, MinFileBytes: 1,
		JournalPath: filepath.Join(t.TempDir(), ".tms-watched"),
		Catalog:     sourceCatalog{src: src},
		Store:       dropper,
		Protected:   func() []string { return protectedFiles(src, session) },
		Now:         func() time.Time { return now },
		Free:        func(string) (int64, error) { return 0, nil },
	})
	old := now.Add(-400 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(store, "Друзья", "s02e04.mkv"), old, old); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"s03e14.mkv", "s01e01.mkv", "s02e03.mkv"} {
		now = now.Add(time.Duration(i+1) * time.Minute)
		keeper.Touch("Друзья/" + name)
	}

	keeper.Sweep()

	for _, kept := range []string{"s02e03.mkv", "s02e04.mkv"} {
		if _, err := os.Stat(filepath.Join(store, "Друзья", kept)); err != nil {
			t.Errorf("выселена %s, выселено всё: %q", kept, dropper.dropped)
		}
	}
	if want := []string{"Друзья/s03e14.mkv", "Друзья/s01e01.mkv"}; !reflect.DeepEqual(dropper.dropped, want) {
		t.Errorf("выселено %q, ожидалось %q", dropper.dropped, want)
	}
}
