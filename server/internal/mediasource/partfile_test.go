package mediasource

import (
	"bytes"
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Тесты про part-файлы: почему хранилище собирается руками (partfile.go).
//
// Два первых ФИКСИРУЮТ ПОВЕДЕНИЕ АПСТРИМА — они утверждают, что баг есть.
// Если они упадут, значит anacrolix его починил: тогда надо перечитать
// partfile.go и решить, нужен ли ещё UsePartFiles=false. Без них третий тест
// проверял бы выдумку: «наше хранилище не портит данные» ничего не стоит,
// пока не показано, чем именно портит умолчание.
//
// Сценарий у всех трёх один и повторяет прод: серия A скачана целиком, серия B
// ещё нет, и общий с ними кусок записывается заново.

type partFileFixture struct {
	// newStorage поднимает хранилище над store — так же, как это делает
	// torrent.NewClient (upstream) или newStore (наше).
	newStorage func(dir string) storage.ClientImplCloser

	info     metainfo.Info
	infoHash metainfo.Hash
	// all — весь торрент как один поток байт, из него берутся данные кусков.
	all []byte
	// dataA — эталонное содержимое первого файла.
	dataA []byte
	// boundary — номер куска, принадлежащего и первому файлу, и второму.
	boundary int
	// store — каталог хранилища, aPath — путь первого файла в нём.
	store string
	aPath string
}

func newPartFileFixture(t *testing.T, newStorage func(dir string) storage.ClientImplCloser) *partFileFixture {
	t.Helper()
	pieceLength := int64(1 << 14)
	// Длина A НЕ кратна длине куска — как у любой серии в реальном паке.
	// Именно из этого и растёт общий кусок у соседних файлов.
	lenA := 3*pieceLength + 100
	lenB := 2 * pieceLength

	src := t.TempDir()
	pack := filepath.Join(src, "pack")
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatal(err)
	}
	dataA, dataB := patternBytes('A', lenA), patternBytes('B', lenB)
	if err := os.WriteFile(filepath.Join(pack, "a.bin"), dataA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "b.bin"), dataB, 0o644); err != nil {
		t.Fatal(err)
	}

	info := metainfo.Info{PieceLength: pieceLength}
	if err := info.BuildFromFilePath(pack); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	store := t.TempDir()
	return &partFileFixture{
		newStorage: newStorage,
		info:       info,
		infoHash:   metainfo.HashBytes(infoBytes),
		all:        append(append([]byte{}, dataA...), dataB...),
		dataA:      dataA,
		boundary:   int((lenA - 1) / pieceLength),
		store:      store,
		aPath:      filepath.Join(store, "pack", "a.bin"),
	}
}

// open поднимает хранилище и открывает в нём торрент. Каждый вызов — это ещё
// и перезапуск процесса: закрытие прежнего повешено на t.Cleanup.
func (f *partFileFixture) open(t *testing.T) storage.TorrentImpl {
	t.Helper()
	cl := f.newStorage(f.store)
	ti, err := cl.OpenTorrent(context.Background(), &f.info, f.infoHash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ti.Close()
		cl.Close()
	})
	return ti
}

// downloadA скачивает первый файл целиком, включая граничный кусок, и убеждается,
// что он лёг на диск целым. Дальше в тестах его уже никто не трогает — всё, что
// с ним случится, случится из-за соседа.
func (f *partFileFixture) downloadA(t *testing.T) {
	t.Helper()
	ti := f.open(t)
	for i := 0; i <= f.boundary; i++ {
		f.writePiece(t, ti, i)
	}
	got, err := os.ReadFile(f.aPath)
	if err != nil {
		t.Fatalf("после скачивания ожидался готовый файл: %v", err)
	}
	if !bytes.Equal(got, f.dataA) {
		t.Fatalf("файл испорчен уже после скачивания")
	}
	t.Logf("скачано: %s", f.layout(t))
}

func (f *partFileFixture) writePiece(t *testing.T, ti storage.TorrentImpl, index int) {
	t.Helper()
	p := ti.Piece(f.info.Piece(index))
	if _, err := p.WriteAt(f.pieceData(t, index), 0); err != nil {
		t.Fatalf("кусок %d: запись: %v", index, err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatalf("кусок %d: MarkComplete: %v", index, err)
	}
}

// pieceData отдаёт данные куска, сверив их с хэшем из метаинформации: иначе
// ошибка в смещениях выглядела бы как найденный баг.
func (f *partFileFixture) pieceData(t *testing.T, index int) []byte {
	t.Helper()
	mp := f.info.Piece(index)
	data := f.all[mp.Offset() : mp.Offset()+mp.Length()]
	want, ok := mp.V1Hash().AsTuple()
	if !ok {
		t.Fatalf("кусок %d: в метаинформации нет хэша", index)
	}
	if sum := sha1.Sum(data); !bytes.Equal(sum[:], want.Bytes()) {
		t.Fatalf("кусок %d: данные не сходятся с хэшем из метаинформации", index)
	}
	return data
}

// layout печатает, что лежит на диске под первым файлом, — сразу под оба имени.
func (f *partFileFixture) layout(t *testing.T) string {
	t.Helper()
	out := ""
	for _, name := range []string{f.aPath, f.aPath + ".part"} {
		st, err := os.Stat(name)
		if err != nil {
			continue
		}
		alloc, _ := allocatedBytes(name)
		out += filepath.Base(name) + " — размер " + humanBytes(st.Size()) +
			", на диске " + humanBytes(alloc) + ", права " + st.Mode().String() + "; "
	}
	if out == "" {
		return "под этим именем на диске ничего нет"
	}
	return out
}

// readWholeA читает первый файл глазами хранилища, а не с диска.
func (f *partFileFixture) readWholeA(t *testing.T, ti storage.TorrentImpl) []byte {
	t.Helper()
	out := make([]byte, 0, len(f.dataA))
	for i := 0; i <= f.boundary; i++ {
		mp := f.info.Piece(i)
		buf := make([]byte, min(mp.Length(), int64(len(f.dataA))-mp.Offset()))
		if _, err := ti.Piece(mp).ReadAt(buf, 0); err != nil {
			t.Fatalf("чтение куска %d: %v", i, err)
		}
		out = append(out, buf...)
	}
	return out
}

func zeroShare(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	return 100 * float64(bytes.Count(b, []byte{0})) / float64(len(b))
}

// TestUpstreamPartFilesHideCompleteFile фиксирует первое следствие part-файлов:
// стоило соседу начать качаться — и готовая серия читается нулями, хотя данные
// ещё целы на диске. Именно так сеанс перекодирования умирает посреди серии,
// не написав ни одной ошибки.
func TestUpstreamPartFilesHideCompleteFile(t *testing.T) {
	f := newPartFileFixture(t, storage.NewFile)
	f.downloadA(t)

	// Перезапуск процесса: setCompletionFromPartFiles снимает отметку
	// готовности с граничного куска, потому что тот принадлежит ещё
	// не скачанному второму файлу. Здесь и заряжается мина.
	ti := f.open(t)
	if c := ti.Piece(f.info.Piece(f.boundary)).Completion(); c.Complete {
		t.Fatalf("АПСТРИМ ИСПРАВЛЕН: после перезапуска граничный кусок %d остался готовым; перечитать partfile.go", f.boundary)
	}

	// Сосед качается: граничный кусок записан, но ещё не помечен готовым.
	if _, err := ti.Piece(f.info.Piece(f.boundary)).WriteAt(f.pieceData(t, f.boundary), 0); err != nil {
		t.Fatal(err)
	}
	t.Logf("после записи граничного куска: %s", f.layout(t))

	onDisk, err := os.ReadFile(f.aPath)
	if err != nil {
		t.Fatalf("готовый файл исчез с диска: %v", err)
	}
	if !bytes.Equal(onDisk, f.dataA) {
		t.Fatalf("АПСТРИМ ИЗМЕНИЛСЯ: на этом шаге данные должны быть ещё целы, а они уже испорчены")
	}

	got := f.readWholeA(t, ti)
	if bytes.Equal(got, f.dataA) {
		t.Fatalf("АПСТРИМ ИСПРАВЛЕН: хранилище отдало готовый файл правильно, хотя рядом лежит пустой .part; перечитать partfile.go")
	}
	t.Logf("данные целы на диске, но хранилище отдаёт %.1f%% нулей", zeroShare(got))
}

// TestUpstreamPartFilesDestroyCompleteFile фиксирует второе следствие:
// MarkComplete на том же куске перекладывает пустышку поверх настоящих данных,
// и они пропадают навсегда. Файл при этом остаётся полного размера
// и read-only — снаружи неотличим от честно скачанного.
func TestUpstreamPartFilesDestroyCompleteFile(t *testing.T) {
	f := newPartFileFixture(t, storage.NewFile)
	f.downloadA(t)

	ti := f.open(t)
	if c := ti.Piece(f.info.Piece(f.boundary)).Completion(); c.Complete {
		t.Fatalf("АПСТРИМ ИСПРАВЛЕН: после перезапуска граничный кусок %d остался готовым; перечитать partfile.go", f.boundary)
	}

	f.writePiece(t, ti, f.boundary)
	t.Logf("после MarkComplete: %s", f.layout(t))

	got, err := os.ReadFile(f.aPath)
	if err != nil {
		t.Fatalf("готовый файл исчез: %v", err)
	}
	if bytes.Equal(got, f.dataA) {
		t.Fatal("АПСТРИМ ИСПРАВЛЕН: готовый файл уцелел после повышения соседского .part; перечитать partfile.go")
	}
	if int64(len(got)) != int64(len(f.dataA)) {
		t.Fatalf("АПСТРИМ ИЗМЕНИЛСЯ: испорченный файл сменил размер (%d вместо %d) — фантом опознавался как раз по совпадению размера",
			len(got), len(f.dataA))
	}
	t.Logf("файл того же размера, но нулей в нём %.1f%%", zeroShare(got))
}

// TestOurStoreSurvivesBoundaryPieceRewrite — то, ради чего хранилище и собрано
// руками: с выключенными part-файлами перезапись общего куска (перезапуск,
// плохой пир, лечение фантомов) готовому файлу ничего не делает.
func TestOurStoreSurvivesBoundaryPieceRewrite(t *testing.T) {
	f := newPartFileFixture(t, newStore)
	f.downloadA(t)

	if _, err := os.Stat(f.aPath + ".part"); err == nil {
		t.Error("рядом с готовым файлом появился .part — part-файлы не выключились")
	}

	ti := f.open(t)
	// Отметка готовности пережила перезапуск: она в .torrent.bolt.db, а не
	// в именах файлов. То есть скачанное не перекачивается.
	if c := ti.Piece(f.info.Piece(f.boundary)).Completion(); !c.Complete {
		t.Error("граничный кусок после перезапуска перестал считаться готовым: серия будет перекачана заново")
	}

	// Перезаписываем его всё равно — так делает и плохой пир, и наше лечение
	// фантомов, и ровно это убивало файл на part-файлах.
	f.writePiece(t, ti, f.boundary)
	t.Logf("после перезаписи граничного куска: %s", f.layout(t))

	got, err := os.ReadFile(f.aPath)
	if err != nil {
		t.Fatalf("готовый файл исчез: %v", err)
	}
	if !bytes.Equal(got, f.dataA) {
		t.Fatalf("файл испорчен: нулей %.1f%%", zeroShare(got))
	}
	if fromStore := f.readWholeA(t, ti); !bytes.Equal(fromStore, f.dataA) {
		t.Fatalf("хранилище отдаёт не то, что лежит на диске: нулей %.1f%%", zeroShare(fromStore))
	}
}

// patternBytes даёт неповторяющиеся данные: одинаковые куски дали бы одинаковые
// хэши и спрятали бы путаницу со смещениями.
func patternBytes(seed byte, n int64) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i%23)
	}
	return out
}
