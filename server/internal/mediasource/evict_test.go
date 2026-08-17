package mediasource

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Тесты выселения. Проверяется ровно то, ошибка в чём стоит дороже всего:
// удалён тот файл, что просили, и отметки готовности сняты. Промах в первом —
// это чужие гигабайты, промах во втором — фантом из phantom.go: торрент
// считает файл скачанным, читатель мгновенно отдаёт нули, и ни одной ошибки
// в логе не появляется.

// withStore открывает хранилище, отдаёт торрент в нём и закрывает за собой.
//
// Закрывать обязательно сразу: база готовности — это один bolt-файл на каталог,
// и второй экземпляр не ждёт первого, а через секунду сдаётся и молча
// подменяется картой в памяти (storage.pieceCompletionForDir). Тест на этом
// не упал бы — он перестал бы проверять то, ради чего написан.
func (f *partFileFixture) withStore(t *testing.T, do func(ti storage.TorrentImpl)) {
	t.Helper()
	cl := f.newStorage(f.store)
	defer cl.Close()
	ti, err := cl.OpenTorrent(context.Background(), &f.info, f.infoHash)
	if err != nil {
		t.Fatal(err)
	}
	defer ti.Close()
	do(ti)
}

// fill скачивает куски [0, last] и закрывает хранилище.
func (f *partFileFixture) fill(t *testing.T, last int) {
	t.Helper()
	f.withStore(t, func(ti storage.TorrentImpl) {
		for i := 0; i <= last; i++ {
			f.writePiece(t, ti, i)
		}
	})
}

// complete отдаёт отметки готовности кусков [0, last].
func (f *partFileFixture) complete(t *testing.T, last int) []bool {
	t.Helper()
	out := make([]bool, 0, last+1)
	f.withStore(t, func(ti storage.TorrentImpl) {
		for i := 0; i <= last; i++ {
			out = append(out, ti.Piece(f.info.Piece(i)).Completion().Complete)
		}
	})
	return out
}

// writeTorrentFile кладёт настоящий .torrent: DropFile берёт метаинформацию
// с диска ровно так же, как это делает библиотека.
func (f *partFileFixture) writeTorrentFile(t *testing.T) string {
	t.Helper()
	infoBytes, err := bencode.Marshal(f.info)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pack.torrent")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).Write(fh); err != nil {
		t.Fatal(err)
	}
	return path
}

// newEvictClient собирает клиент без сети: выселению нужны только хранилище
// и каталог данных.
func newEvictClient(t *testing.T, store string) *Client {
	t.Helper()
	c := &Client{store: newStore(store), dataDir: store}
	t.Cleanup(func() { closeStore(c) })
	return c
}

// closeStore закрывает хранилище клиента и делает повторный вызов безвредным.
// Закрывать посреди теста приходится потому, что проверка отметок открывает
// bolt заново, а он один на каталог.
func closeStore(c *Client) {
	if c.store != nil {
		c.store.Close()
		c.store = nil
	}
}

// TestStoreFilesMatchesRealLayout — сверка нашей раскладки с настоящей.
//
// StoreFiles повторяет вычисления anacrolix, потому что спросить их не у кого:
// в клиенте лежит только активный торрент, а место занимают все. Расхождение
// означало бы удаление не того файла, поэтому пути сверяются с тем, куда
// хранилище действительно записало данные.
func TestStoreFilesMatchesRealLayout(t *testing.T) {
	f := newPartFileFixture(t, newStore)
	f.fill(t, f.info.NumPieces()-1)

	files := StoreFiles(&f.info)
	if len(files) != 2 {
		t.Fatalf("файлов %d, ожидалось 2", len(files))
	}

	for _, sf := range files {
		full := filepath.Join(f.store, filepath.FromSlash(sf.Rel))
		st, err := os.Stat(full)
		if err != nil {
			t.Fatalf("%q: хранилище положило данные не сюда: %v", sf.Rel, err)
		}
		if st.Size() != sf.Length {
			t.Errorf("%q: на диске %d байт, в метаинформации %d", sf.Rel, st.Size(), sf.Length)
		}
	}
	if got := filepath.Join(f.store, filepath.FromSlash(files[0].Rel)); got != f.aPath {
		t.Errorf("путь первого файла %q, ожидался %q", got, f.aPath)
	}

	// Граничный кусок принадлежит обоим файлам — из этого растёт вся возня
	// с part-файлами (partfile.go) и с ним же связана цена выселения:
	// сосед потеряет один кусок и докачает его.
	if files[0].LastPiece != f.boundary || files[1].FirstPiece != f.boundary {
		t.Errorf("границы кусков: файл A [%d, %d], файл B [%d, %d], общим ожидался %d",
			files[0].FirstPiece, files[0].LastPiece,
			files[1].FirstPiece, files[1].LastPiece, f.boundary)
	}
}

// TestDropFileRemovesDataAndCompletion — основной путь: файла нет, отметок нет.
func TestDropFileRemovesDataAndCompletion(t *testing.T) {
	f := newPartFileFixture(t, newStore)
	f.fill(t, f.info.NumPieces()-1)

	torrentPath := f.writeTorrentFile(t)
	before, err := allocatedBytes(f.aPath)
	if err != nil {
		t.Fatal(err)
	}

	c := newEvictClient(t, f.store)
	freed, err := c.DropFile(torrentPath, 0)
	if err != nil {
		t.Fatalf("DropFile: %v", err)
	}
	if freed != before {
		t.Errorf("освобождено %d, на диске файл занимал %d", freed, before)
	}
	if _, err := os.Stat(f.aPath); !os.IsNotExist(err) {
		t.Errorf("файл остался на диске: %v", err)
	}

	// Хранилище клиента закрывается до проверки: bolt один на каталог.
	closeStore(c)

	for i, complete := range f.complete(t, f.info.NumPieces()-1) {
		want := i > f.boundary
		if complete != want {
			t.Errorf("кусок %d: готов=%v, ожидалось %v (граничный — %d)",
				i, complete, want, f.boundary)
		}
	}
}

// TestDropFileSurvivesMissingData — выселять можно и то, чего на диске уже нет.
//
// Так выглядит файл, стёртый руками на VPS: отметки готовности остались
// в bolt и живут своей жизнью. Отказ здесь означал бы, что вычистить их
// нечем, и торрент до перезапуска считал бы файл скачанным.
func TestDropFileSurvivesMissingData(t *testing.T) {
	f := newPartFileFixture(t, newStore)
	f.fill(t, f.info.NumPieces()-1)
	if err := os.Remove(f.aPath); err != nil {
		t.Fatal(err)
	}

	torrentPath := f.writeTorrentFile(t)
	c := newEvictClient(t, f.store)
	freed, err := c.DropFile(torrentPath, 0)
	if err != nil {
		t.Fatalf("DropFile: %v", err)
	}
	if freed != 0 {
		t.Errorf("освобождено %d, а файла и так не было", freed)
	}

	closeStore(c)
	if f.complete(t, f.boundary)[f.boundary] {
		t.Error("граничный кусок остался готовым: это и есть фантом")
	}
}

// TestDropFileRefusesPathsOutsideStore — путь собирается из недоверенного
// .torrent, и промах здесь означает удаление чужого файла на VPS.
func TestDropFileRefusesPathsOutsideStore(t *testing.T) {
	c := &Client{dataDir: t.TempDir()}
	for _, rel := range []string{
		"../соседний-каталог/файл.mkv",
		"..",
		"пак/../../побег.mkv",
		".",
	} {
		if got, err := c.storePath(rel); err == nil {
			t.Errorf("%q: путь принят как %q, ожидался отказ", rel, got)
		}
	}
	if _, err := c.storePath("пак/Сезон 01/s01e01.mkv"); err != nil {
		t.Errorf("обычный путь отвергнут: %v", err)
	}
}
