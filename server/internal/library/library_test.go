package library

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/avdav/torrent-media/server/internal/mediasource"
)

// stubSource — источник, который умеет только сказать, закрыли ли его.
// Настоящий mediasource.Torrent сюда не годится: он тянет за собой клиент,
// рой и сеть, а проверяется здесь порядок открытия и закрытия.
type stubSource struct {
	path   string
	closed bool
}

func (s *stubSource) Ready() bool                          { return true }
func (s *stubSource) Name() string                         { return filepath.Base(s.path) }
func (s *stubSource) Files() []mediasource.File            { return make([]mediasource.File, 0) }
func (s *stubSource) Open(int) (mediasource.Reader, error) { return nil, errors.New("не нужно") }
func (s *stubSource) Stats() mediasource.Stats             { return mediasource.Stats{} }
func (s *stubSource) Close() error                         { s.closed = true; return nil }

// opener собирает OpenFunc и запоминает выданные источники.
func opener(t *testing.T) (OpenFunc, *[]*stubSource) {
	t.Helper()
	var opened []*stubSource
	return func(path string) (mediasource.Source, error) {
		s := &stubSource{path: path}
		opened = append(opened, s)
		return s, nil
	}, &opened
}

// torrentBytes собирает настоящий .torrent вокруг файла с заданным содержимым.
// Именно настоящий, а не заглушка: библиотека считает по нему infohash,
// и подделка не проверила бы ни дедупликацию, ни разбор.
func torrentBytes(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 1 << 14}
	if err := info.BuildFromFilePath(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: raw}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAddAndList(t *testing.T) {
	dir := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, t.TempDir(), open)

	data := torrentBytes(t, "s01e01.mkv", []byte("первая серия"))
	entry, err := lib.Add(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if entry.Name != "s01e01.mkv" {
		t.Errorf("Name = %q, ожидалось имя из метаинформации", entry.Name)
	}
	if len(entry.ID) != 40 {
		t.Errorf("ID = %q, ожидался infohash в hex", entry.ID)
	}
	if entry.Files != 1 || entry.Length != int64(len("первая серия")) {
		t.Errorf("Files/Length = %d/%d, ожидалось 1/%d", entry.Files, entry.Length, len("первая серия"))
	}

	// Файл должен лечь в каталог под читаемым именем: библиотеку разбирают
	// руками с VPS, и <infohash>.torrent там бесполезен.
	if _, err := os.Stat(filepath.Join(dir, "s01e01.mkv.torrent")); err != nil {
		t.Errorf("файл не лёг в каталог: %v", err)
	}

	list, err := lib.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != entry.ID {
		t.Fatalf("List вернул %+v", list)
	}
	if list[0].Active {
		t.Error("торрент помечен активным, хотя его не включали")
	}
}

// TestAddIsIdempotent: «загрузил ещё раз, потому что не понял, дошло ли»
// не должно давать вторую запись — иначе список на телефоне зарастёт.
func TestAddIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, t.TempDir(), open)

	data := torrentBytes(t, "s01e01.mkv", []byte("первая серия"))
	first, err := lib.Add(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	second, err := lib.Add(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("id разъехались: %s и %s", first.ID, second.ID)
	}

	list, _ := lib.List()
	if len(list) != 1 {
		t.Fatalf("в библиотеке %d записей, ожидалась одна", len(list))
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.torrent"))
	if len(names) != 1 {
		t.Errorf("на диске %d файлов: %v", len(names), names)
	}
}

func TestAddRejectsGarbage(t *testing.T) {
	lib := New(t.TempDir(), t.TempDir(), func(string) (mediasource.Source, error) { return nil, nil })

	if _, err := lib.Add(strings.NewReader("это не торрент")); !errors.Is(err, ErrBadTorrent) {
		t.Errorf("ошибка = %v, ожидалась ErrBadTorrent", err)
	}
	if _, err := lib.Add(bytes.NewReader(make([]byte, MaxTorrentBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ошибка = %v, ожидалась ErrTooLarge", err)
	}
	names, _ := filepath.Glob(filepath.Join(lib.Dir(), "*"))
	if len(names) != 0 {
		t.Errorf("в каталог что-то попало: %v", names)
	}
}

// TestActivateSwitchesSource — главный сценарий: включение второго торрента
// поднимает новый источник и закрывает прежний. Незакрытый источник означал бы
// живой торрент в клиенте, который продолжает качать.
func TestActivateSwitchesSource(t *testing.T) {
	dir := t.TempDir()
	open, opened := opener(t)
	lib := New(dir, t.TempDir(), open)

	first, _ := lib.Add(bytes.NewReader(torrentBytes(t, "a.mkv", []byte("первый"))))
	second, _ := lib.Add(bytes.NewReader(torrentBytes(t, "b.mkv", []byte("второй"))))

	if _, err := lib.Activate(first.ID); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if lib.ActiveID() != first.ID {
		t.Fatalf("активен %q, ожидался %q", lib.ActiveID(), first.ID)
	}
	if lib.Current() == nil {
		t.Fatal("Current пуст после активации")
	}

	if _, err := lib.Activate(second.ID); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(*opened) != 2 {
		t.Fatalf("открыто источников: %d, ожидалось 2", len(*opened))
	}
	if !(*opened)[0].closed {
		t.Error("прежний источник не закрыт: торрент остался в клиенте")
	}
	if (*opened)[1].closed {
		t.Error("новый источник закрыт")
	}

	list, _ := lib.List()
	for _, e := range list {
		if (e.ID == second.ID) != e.Active {
			t.Errorf("флаг active у %q = %v", e.Name, e.Active)
		}
	}
}

// TestActivateKeepsPreviousOnFailure: неудачное переключение не должно
// оставлять сервер вообще без торрента.
func TestActivateKeepsPreviousOnFailure(t *testing.T) {
	dir := t.TempDir()
	var fail bool
	var opened []*stubSource
	lib := New(dir, t.TempDir(), func(path string) (mediasource.Source, error) {
		if fail {
			return nil, errors.New("торрент не добавился")
		}
		s := &stubSource{path: path}
		opened = append(opened, s)
		return s, nil
	})

	first, _ := lib.Add(bytes.NewReader(torrentBytes(t, "a.mkv", []byte("первый"))))
	second, _ := lib.Add(bytes.NewReader(torrentBytes(t, "b.mkv", []byte("второй"))))
	if _, err := lib.Activate(first.ID); err != nil {
		t.Fatal(err)
	}

	fail = true
	if _, err := lib.Activate(second.ID); err == nil {
		t.Fatal("Activate не вернул ошибку")
	}
	if lib.ActiveID() != first.ID {
		t.Errorf("активен %q, ожидался прежний %q", lib.ActiveID(), first.ID)
	}
	if opened[0].closed {
		t.Error("прежний источник закрыт, хотя переключение не состоялось")
	}
}

// TestRestorePrefersSavedChoice — ради этого выбор и пишется на диск:
// выкатка не должна возвращать телевизор на торрент из аргумента запуска.
func TestRestorePrefersSavedChoice(t *testing.T) {
	dir := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, t.TempDir(), open)

	seedData := torrentBytes(t, "seed.mkv", []byte("из аргумента"))
	seedPath := filepath.Join(t.TempDir(), "seed.torrent")
	if err := os.WriteFile(seedPath, seedData, 0o644); err != nil {
		t.Fatal(err)
	}
	chosen, _ := lib.Add(bytes.NewReader(torrentBytes(t, "chosen.mkv", []byte("выбранный"))))

	if _, err := lib.Restore(seedPath); err != nil {
		t.Fatalf("первый Restore: %v", err)
	}
	if lib.ActiveID() == chosen.ID {
		t.Fatal("без сохранённого выбора должен был включиться seed")
	}
	if _, err := lib.Activate(chosen.ID); err != nil {
		t.Fatal(err)
	}

	// Перезапуск процесса: новая библиотека над тем же каталогом.
	open2, _ := opener(t)
	restarted := New(dir, t.TempDir(), open2)
	entry, err := restarted.Restore(seedPath)
	if err != nil {
		t.Fatalf("Restore после перезапуска: %v", err)
	}
	if entry.ID != chosen.ID || restarted.ActiveID() != chosen.ID {
		t.Errorf("после перезапуска активен %q, ожидался выбранный %q", restarted.ActiveID(), chosen.ID)
	}
}

// TestRestoreFallsBackWhenSavedGone — .torrent могли удалить руками с VPS.
func TestRestoreFallsBackWhenSavedGone(t *testing.T) {
	dir := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, t.TempDir(), open)

	gone, _ := lib.Add(bytes.NewReader(torrentBytes(t, "gone.mkv", []byte("исчезнет"))))
	if _, err := lib.Activate(gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, gone.File)); err != nil {
		t.Fatal(err)
	}

	seedData := torrentBytes(t, "seed.mkv", []byte("из аргумента"))
	seedPath := filepath.Join(t.TempDir(), "seed.torrent")
	if err := os.WriteFile(seedPath, seedData, 0o644); err != nil {
		t.Fatal(err)
	}

	open2, _ := opener(t)
	restarted := New(dir, t.TempDir(), open2)
	entry, err := restarted.Restore(seedPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if entry.ID == "" || entry.ID == gone.ID {
		t.Errorf("активен %q, ожидался seed", entry.ID)
	}
}

// TestRestoreWithEmptyLibrary: пустая библиотека — не ошибка. Сервер обязан
// подняться и ждать первой загрузки с телефона.
func TestRestoreWithEmptyLibrary(t *testing.T) {
	open, _ := opener(t)
	lib := New(t.TempDir(), t.TempDir(), open)

	entry, err := lib.Restore("")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if entry.ID != "" || lib.Current() != nil {
		t.Errorf("на пустой библиотеке что-то активировалось: %+v", entry)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	store := t.TempDir()
	open, opened := opener(t)
	lib := New(dir, store, open)

	entry, _ := lib.Add(bytes.NewReader(torrentBytes(t, "a.mkv", []byte("данные"))))
	if _, err := lib.Activate(entry.ID); err != nil {
		t.Fatal(err)
	}
	// Скачанное лежит в <store>/<имя торрента>.
	payload := filepath.Join(store, entry.Name)
	if err := os.WriteFile(payload, []byte("скачанное"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := lib.Remove(entry.ID, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if lib.ActiveID() != "" || lib.Current() != nil {
		t.Error("удалили активный торрент, а он всё ещё активен")
	}
	if !(*opened)[0].closed {
		t.Error("источник удалённого торрента не закрыт")
	}
	if _, err := os.Stat(filepath.Join(dir, entry.File)); !os.IsNotExist(err) {
		t.Error(".torrent остался в каталоге")
	}
	if _, err := os.Stat(payload); err != nil {
		t.Error("без ?data=1 скачанное трогать нельзя")
	}

	if err := lib.Remove(entry.ID, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление: %v, ожидалась ErrNotFound", err)
	}
}

func TestRemoveWithData(t *testing.T) {
	dir := t.TempDir()
	store := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, store, open)

	entry, _ := lib.Add(bytes.NewReader(torrentBytes(t, "a.mkv", []byte("данные"))))
	payload := filepath.Join(store, entry.Name)
	if err := os.WriteFile(payload, []byte("скачанное"), 0o644); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(store, "чужой торрент")
	if err := os.WriteFile(neighbour, []byte("не трогать"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := lib.Remove(entry.ID, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Error("скачанное должно было удалиться")
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Error("удаление задело соседний торрент")
	}
}

// TestRemoveDataRejectsUnsafeName: имя приходит из недоверенного .torrent,
// и промах здесь означает удаление чужого каталога на VPS.
func TestRemoveDataRejectsUnsafeName(t *testing.T) {
	store := t.TempDir()
	victim := filepath.Join(filepath.Dir(store), "не-наш-каталог")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := New(t.TempDir(), store, func(string) (mediasource.Source, error) { return nil, nil })

	for _, name := range []string{"..", ".", "", "../не-наш-каталог", "a/b", `a\b`} {
		if err := lib.removeDataLocked(name); err == nil {
			t.Errorf("имя %q принято, а должно было быть отвергнуто", name)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("соседний каталог пострадал: %v", err)
	}
}

// TestFileNameFor — имя файла делается из имени торрента, то есть из данных,
// которые прислал кто угодно.
func TestFileNameFor(t *testing.T) {
	// Собирается из повтора, а не пишется литералом: скан секретов в CI
	// (и парный ему pre-commit-хук) ругается на 40+ hex-символов подряд —
	// токен обратного прокси выглядит ровно так же.
	id := strings.Repeat("a1", 20)
	cases := []struct{ in, want string }{
		{"Сериал S01", "Сериал S01.torrent"},
		// Точки по краям срезаются вместе с разделителями: иначе имя вроде
		// «..» превратилось бы в ссылку на родительский каталог.
		{"../../etc/passwd", "_.._etc_passwd.torrent"},
		{"/etc/shadow", "_etc_shadow.torrent"},
		{".tms-active", "tms-active.torrent"},
		{"  ", id + ".torrent"},
		{"..", id + ".torrent"},
		{"со\x00служебным\tсимволом", "со_служебным_символом.torrent"},
	}
	for _, c := range cases {
		if got := fileNameFor(c.in, id); got != c.want {
			t.Errorf("fileNameFor(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
	for _, c := range cases {
		got := fileNameFor(c.in, id)
		if filepath.Base(got) != got || strings.HasPrefix(got, ".") {
			t.Errorf("fileNameFor(%q) = %q: не один сегмент пути или скрытый файл", c.in, got)
		}
	}
}

// packBytes собирает .torrent на КАТАЛОГ из нескольких файлов — так приезжает
// сериал. Одиночный файл (torrentBytes выше) раскладывается на диске иначе,
// и оба случая нужны: у пака путь это «имя раздачи/путь внутри», а у одиночного
// имя раздачи и есть весь путь.
func packBytes(t *testing.T, dirName string, names ...string) []byte {
	t.Helper()
	root := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// Разной длины, чтобы перепутанные файлы было видно по размеру.
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte('a' + i)}, 100*(i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 1 << 14}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: raw}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStoredFilesCoversWholeLibrary — чистка выселяет по этому списку, поэтому
// в нём обязаны быть файлы ВСЕХ торрентов, а не только активного: место
// занимают все, а в клиенте лежит один.
func TestStoredFilesCoversWholeLibrary(t *testing.T) {
	dir := t.TempDir()
	open, _ := opener(t)
	lib := New(dir, t.TempDir(), open)

	pack, err := lib.Add(bytes.NewReader(packBytes(t, "Сериал", "Сезон 01/s01e01.mkv", "Сезон 01/s01e02.mkv")))
	if err != nil {
		t.Fatal(err)
	}
	single, err := lib.Add(bytes.NewReader(torrentBytes(t, "фильм.mkv", []byte("одиночный файл"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Activate(pack.ID); err != nil {
		t.Fatal(err)
	}

	files, err := lib.StoredFiles()
	if err != nil {
		t.Fatalf("StoredFiles: %v", err)
	}

	got := make(map[string]StoredFile, len(files))
	for _, f := range files {
		got[f.Rel] = f
	}
	// Путь у пака — с именем раздачи впереди, у одиночного файла имя раздачи
	// и есть весь путь. Удвоение («фильм.mkv/фильм.mkv») означало бы, что
	// чистка ищет файл не там, где он лежит, и не выселит ничего.
	want := []string{"Сериал/Сезон 01/s01e01.mkv", "Сериал/Сезон 01/s01e02.mkv", "фильм.mkv"}
	if len(files) != len(want) {
		t.Fatalf("файлов %d, ожидалось %d: %v", len(files), len(want), got)
	}
	for _, rel := range want {
		f, ok := got[rel]
		if !ok {
			t.Fatalf("файла %q в списке нет: %v", rel, got)
		}
		if f.Length <= 0 {
			t.Errorf("%q: длина %d", rel, f.Length)
		}
		if _, err := os.Stat(f.TorrentFile); err != nil {
			t.Errorf("%q: .torrent не найден: %v", rel, err)
		}
	}
	if !got["Сериал/Сезон 01/s01e01.mkv"].Active {
		t.Error("файл активного торрента не помечен активным")
	}
	if got["фильм.mkv"].Active || got["фильм.mkv"].TorrentID != single.ID {
		t.Error("файл неактивного торрента приписан не тому торренту")
	}
}
