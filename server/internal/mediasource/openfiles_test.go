package mediasource

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func sameIndices(t *testing.T, what string, got, want []int) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: открыты %v, ожидалось %v", what, got, want)
	}
}

// TestFakeTracksOpenFiles — файл открыт, пока жив хоть один его Reader; повторный
// Close не должен отпустить чужой Reader того же файла.
func TestFakeTracksOpenFiles(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"s01e01.mkv", "s01e02.mkv", "s01e03.mkv"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	src, err := NewFake("Друзья", paths...)
	if err != nil {
		t.Fatal(err)
	}

	sameIndices(t, "до чтения", src.OpenFiles(), nil)
	probe, err := src.Open(2)
	if err != nil {
		t.Fatal(err)
	}
	ffmpeg, err := src.Open(2)
	if err != nil {
		t.Fatal(err)
	}
	prebuffer, err := src.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	sameIndices(t, "три читателя", src.OpenFiles(), []int{0, 2})

	probe.Close()
	probe.Close()
	sameIndices(t, "закрыт один из двух читателей файла 2", src.OpenFiles(), []int{0, 2})

	ffmpeg.Close()
	sameIndices(t, "закрыты оба читателя файла 2", src.OpenFiles(), []int{0})

	prebuffer.Close()
	sameIndices(t, "всё закрыто", src.OpenFiles(), nil)
}

// TestTorrentTracksOpenFiles — то же на настоящем клиенте: /raw и префетч
// открывают файл через Torrent.Open, и учёт обязан идти именно там.
func TestTorrentTracksOpenFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Друзья")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s01e01.mkv", "s01e02.mkv"} {
		if err := os.WriteFile(filepath.Join(root, name), patternBytes('F', 64<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 32 << 10}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(t.TempDir(), "friends.torrent")
	out, err := os.Create(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).Write(out); err != nil {
		t.Fatal(err)
	}
	out.Close()

	dir := t.TempDir()
	cfg := loopbackConfig(dir)
	cfg.DisableTrackers = true
	store := newStore(dir)
	cfg.DefaultStorage = store
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{client: cl, store: store, dataDir: dir, persistStore: true, readahead: 64 << 10}
	t.Cleanup(func() { c.Close() })
	src, err := c.Add(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	waitFor(t, "метаданные торрента", src.Ready)

	r, err := src.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	sameIndices(t, "открыт Reader", src.OpenFiles(), []int{1})
	r.Close()
	r.Close()
	sameIndices(t, "Reader закрыт", src.OpenFiles(), nil)
}
