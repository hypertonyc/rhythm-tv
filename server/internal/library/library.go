// Package library держит набор .torrent-файлов и помнит, какой из них активен.
//
// Библиотека — это просто КАТАЛОГ с .torrent-файлами, а не своя база: список
// строится сканированием, загрузка с телефона кладёт туда ещё один файл,
// а выбор активного лежит рядом в служебном файле .tms-active. Так каталог
// остаётся тем же, чем был (в него и раньше руками клали .torrent), и любую
// операцию можно повторить из шелла на VPS.
//
// Активен всегда РОВНО ОДИН торрент: сервер держит один сеанс перекодирования
// и одну модель «список серий» для телевизора. Переключение закрывает прежний
// Source, а скачанное на диске не трогает.
package library

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/avdav/torrent-media/server/internal/mediasource"
)

// activeFile — имя служебного файла с id активного торрента.
// Точка в начале не косметика: скан библиотеки ищет *.torrent, а руками
// в каталог кладут именно их, и служебный файл не должен мозолить глаза.
const activeFile = ".tms-active"

// MaxTorrentBytes — предел на загружаемый .torrent.
//
// Метаинформация сериала на 27 ГБ — это сотни килобайт хэшей кусков, так что
// 4 МБ хватает с большим запасом. Предел нужен потому, что эндпоинт загрузки
// собственной авторизации не имеет (её даёт токен в пути на reverse-proxy).
const MaxTorrentBytes = 4 << 20

var (
	// ErrNotFound — такого id в каталоге нет.
	ErrNotFound = errors.New("torrent not found")
	// ErrBadTorrent — файл не разбирается как метаинформация.
	ErrBadTorrent = errors.New("not a valid .torrent file")
	// ErrTooLarge — файл больше MaxTorrentBytes.
	ErrTooLarge = errors.New("torrent file is too large")
)

// Entry — одна запись библиотеки. Ровно это уезжает в JSON /api/torrents.
type Entry struct {
	// ID — infohash в hex. Именно он, а не имя файла: один и тот же торрент,
	// загруженный дважды под разными именами, должен остаться одной записью.
	ID string `json:"id"`
	// Name — имя из метаинформации, то же самое, что потом отдаст /api/files.
	Name string `json:"name"`
	// File — имя .torrent-файла в каталоге; нужно, чтобы его можно было
	// найти руками на VPS.
	File string `json:"file"`
	// Length — суммарный размер содержимого торрента.
	Length int64 `json:"length"`
	// Files — сколько файлов внутри (всех, не только видео).
	Files int `json:"files"`
	// AddedAt — mtime .torrent-файла в миллисекундах.
	AddedAt int64 `json:"addedAt"`
	// Active — этот торрент сейчас обслуживается сервером.
	Active bool `json:"active"`
}

// OpenFunc добавляет торрент в клиент и отдаёт его как источник.
// Обычно это метод mediasource.Client.Add, в тестах — заглушка.
type OpenFunc func(torrentPath string) (mediasource.Source, error)

// Library — каталог с .torrent-файлами плюс выбранный из них активный.
type Library struct {
	dir      string
	storeDir string
	open     OpenFunc

	// mu защищает и активный источник, и кэш разбора. Лок берётся на всё
	// время переключения, включая добавление торрента в клиент: операция
	// редкая (нажатие на телефоне), а гонка «два переключения разом»
	// оставила бы висеть лишний источник.
	mu       sync.Mutex
	activeID string
	active   mediasource.Source
	cache    map[string]cached
}

// cached — разобранная метаинформация одного файла. Ключ кэша — путь,
// признак свежести — mtime и размер: список опрашивается страницей раз
// в секунду, а разбор торрента на 27 ГБ это сотни килобайт хэшей.
type cached struct {
	modTime int64
	size    int64
	entry   Entry
}

// New собирает библиотеку над каталогом dir.
//
// storeDir нужен только для удаления скачанного вместе с торрентом; пустая
// строка отключает эту возможность.
func New(dir, storeDir string, open OpenFunc) *Library {
	return &Library{dir: dir, storeDir: storeDir, open: open, cache: make(map[string]cached)}
}

// Dir отдаёт каталог библиотеки.
func (l *Library) Dir() string { return l.dir }

// Current — активный источник или nil, если ничего не выбрано.
//
// nil здесь — законное состояние, а не ошибка: пустая библиотека выглядит
// для телевизора точно так же, как ещё не загруженные метаданные.
func (l *Library) Current() mediasource.Source {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// ActiveID отдаёт id активного торрента или пустую строку.
func (l *Library) ActiveID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activeID
}

// List сканирует каталог и отдаёт записи, отсортированные по имени.
func (l *Library) List() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listLocked()
}

func (l *Library) listLocked() ([]Entry, error) {
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, de := range names {
		if de.IsDir() || !strings.EqualFold(filepath.Ext(de.Name()), ".torrent") {
			continue
		}
		entry, err := l.entryFor(filepath.Join(l.dir, de.Name()))
		if err != nil {
			// Битый файл не должен ронять весь список: рядом лежат рабочие.
			continue
		}
		// Один и тот же торрент под двумя именами — одна запись; побеждает
		// первая по алфавиту, потому что каталог уже отсортирован.
		if _, dup := seen[entry.ID]; dup {
			continue
		}
		seen[entry.ID] = struct{}{}
		entry.Active = entry.ID == l.activeID
		out = append(out, entry)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// entryFor разбирает один .torrent, используя кэш.
func (l *Library) entryFor(path string) (Entry, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Entry{}, err
	}
	modTime, size := st.ModTime().UnixMilli(), st.Size()
	if c, ok := l.cache[path]; ok && c.modTime == modTime && c.size == size {
		return c.entry, nil
	}

	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrBadTorrent, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrBadTorrent, err)
	}

	entry := Entry{
		ID:      mi.HashInfoBytes().HexString(),
		Name:    info.BestName(),
		File:    filepath.Base(path),
		Length:  info.TotalLength(),
		Files:   len(info.UpvertedFiles()),
		AddedAt: modTime,
	}
	l.cache[path] = cached{modTime: modTime, size: size, entry: entry}
	return entry, nil
}

// Add принимает .torrent из запроса и кладёт его в каталог.
//
// Повторная загрузка того же торрента — не ошибка и не дубль: возвращается
// уже существующая запись. Так «загрузил ещё раз, потому что не понял,
// дошло ли» не засоряет библиотеку.
func (l *Library) Add(r io.Reader) (Entry, error) {
	// +1 байт, чтобы отличить «ровно предел» от «больше предела».
	data, err := io.ReadAll(io.LimitReader(r, MaxTorrentBytes+1))
	if err != nil {
		return Entry{}, err
	}
	if int64(len(data)) > MaxTorrentBytes {
		return Entry{}, ErrTooLarge
	}

	// Разбор ДО записи на диск: в каталог не должно попадать ничего,
	// что мы потом не сможем прочитать.
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrBadTorrent, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrBadTorrent, err)
	}
	id := mi.HashInfoBytes().HexString()

	l.mu.Lock()
	defer l.mu.Unlock()

	existing, err := l.listLocked()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range existing {
		if e.ID == id {
			return e, nil
		}
	}

	name := fileNameFor(info.BestName(), id)
	taken := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		taken[strings.ToLower(e.File)] = struct{}{}
	}
	if _, busy := taken[strings.ToLower(name)]; busy {
		name = strings.TrimSuffix(name, ".torrent") + "-" + id[:8] + ".torrent"
	}

	path := filepath.Join(l.dir, name)
	if err := writeFileAtomic(path, data); err != nil {
		return Entry{}, err
	}
	return l.entryFor(path)
}

// Activate делает торрент активным: поднимает новый источник и закрывает прежний.
//
// Порядок несущий: новый источник открывается ПЕРВЫМ, и если это не удалось,
// активным остаётся прежний. Иначе неудачное переключение оставляло бы
// сервер вообще без торрента.
//
// Прежний источник закрывается ПОСЛЕ снятия лока: Drop у anacrolix дожидается
// своих горутин, а под этим же локом сидит Current(), то есть каждый запрос.
func (l *Library) Activate(id string) (Entry, error) {
	l.mu.Lock()
	entry, prev, err := l.activateLocked(id)
	l.mu.Unlock()

	if prev != nil {
		// Закрытие снимает торрент с клиента и будит всех его читателей.
		// Скачанное на диске остаётся: хранилище общее на всю библиотеку.
		_ = prev.Close()
	}
	return entry, err
}

func (l *Library) activateLocked(id string) (Entry, mediasource.Source, error) {
	entries, err := l.listLocked()
	if err != nil {
		return Entry{}, nil, err
	}
	target := findEntry(entries, id)
	if target == nil {
		return Entry{}, nil, ErrNotFound
	}
	if l.activeID == id && l.active != nil {
		return *target, nil, nil
	}

	source, err := l.open(filepath.Join(l.dir, target.File))
	if err != nil {
		return Entry{}, nil, err
	}

	prev := l.active
	l.active = source
	l.activeID = id
	l.saveActiveLocked(id)

	target.Active = true
	return *target, prev, nil
}

// findEntry ищет запись по id в уже собранном списке.
func findEntry(entries []Entry, id string) *Entry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

// Remove убирает торрент из библиотеки. withData удаляет ещё и скачанное.
//
// Активный торрент удалить можно — он просто перестаёт быть активным, и сервер
// остаётся без источника (для телевизора это выглядит как «метаданные ещё
// грузятся»). Останавливать сеанс перекодирования обязан вызывающий: библиотека
// про ffmpeg ничего не знает.
func (l *Library) Remove(id string, withData bool) error {
	l.mu.Lock()
	// Как и в Activate: закрытие источника вынесено из-под лока, потому что
	// под ним же сидит Current() — то есть каждый входящий запрос.
	prev, err := l.removeLocked(id, withData)
	l.mu.Unlock()

	if prev != nil {
		_ = prev.Close()
	}
	return err
}

func (l *Library) removeLocked(id string, withData bool) (mediasource.Source, error) {
	entries, err := l.listLocked()
	if err != nil {
		return nil, err
	}
	target := findEntry(entries, id)
	if target == nil {
		return nil, ErrNotFound
	}

	var prev mediasource.Source
	if l.activeID == id {
		prev = l.active
		l.active = nil
		l.activeID = ""
		l.saveActiveLocked("")
	}

	path := filepath.Join(l.dir, target.File)
	if err := os.Remove(path); err != nil {
		return prev, err
	}
	delete(l.cache, path)

	if withData {
		if err := l.removeDataLocked(target.Name); err != nil {
			return prev, fmt.Errorf("торрент удалён, но данные остались: %w", err)
		}
	}
	return prev, nil
}

// removeDataLocked стирает скачанное этим торрентом.
//
// Имя каталога берётся из метаинформации, то есть из недоверенного источника,
// поэтому проверяется до последнего: только один сегмент пути, без «..»
// и без разделителей. Ошибиться здесь — это удалить чужой каталог на VPS.
func (l *Library) removeDataLocked(name string) error {
	if l.storeDir == "" {
		return errors.New("хранилище не задано")
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("небезопасное имя торрента %q", name)
	}
	target := filepath.Join(l.storeDir, name)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return nil
}

// Restore восстанавливает активный торрент после перезапуска процесса.
//
// Порядок: сохранённый выбор, затем seed (путь из аргумента командной строки,
// если он задан), затем ничего. Возвращает запись активного торрента или
// нулевую, если активировать нечего — пустая библиотека это не ошибка.
func (l *Library) Restore(seedPath string) (Entry, error) {
	if saved := l.loadActive(); saved != "" {
		if e, err := l.Activate(saved); err == nil {
			return e, nil
		} else if !errors.Is(err, ErrNotFound) {
			return Entry{}, err
		}
		// Сохранённый торрент из каталога исчез — не повод падать,
		// идём дальше по списку кандидатов.
	}

	if seedPath != "" {
		e, err := l.Import(seedPath)
		if err != nil {
			return Entry{}, err
		}
		return l.Activate(e.ID)
	}

	return Entry{}, nil
}

// Import заносит в библиотеку .torrent, лежащий где-то ещё (аргумент
// командной строки). Файл внутри каталога библиотеки просто разбирается
// на месте — копия рядом с оригиналом никому не нужна.
func (l *Library) Import(path string) (Entry, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Entry{}, err
	}
	dir, err := filepath.Abs(l.dir)
	if err != nil {
		return Entry{}, err
	}
	if filepath.Dir(abs) == dir {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.entryFor(abs)
	}

	f, err := os.Open(abs)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	return l.Add(f)
}

// Close закрывает активный источник.
func (l *Library) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == nil {
		return nil
	}
	err := l.active.Close()
	l.active = nil
	return err
}

func (l *Library) loadActive() string {
	data, err := os.ReadFile(filepath.Join(l.dir, activeFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveActiveLocked запоминает выбор. Ошибка записи не отменяет переключение:
// каталог может быть смонтирован только на чтение, и тогда выбор просто
// не переживёт перезапуск — это лучше, чем отказ переключаться вовсе.
func (l *Library) saveActiveLocked(id string) {
	path := filepath.Join(l.dir, activeFile)
	if id == "" {
		_ = os.Remove(path)
		return
	}
	if err := writeFileAtomic(path, []byte(id+"\n")); err != nil {
		log.Printf("не удалось запомнить активный торрент в %s: %v", path, err)
	}
}

// writeFileAtomic пишет через временный файл и rename: недописанный .torrent
// в каталоге библиотеки выглядел бы как битая запись при первом же сканировании.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tms-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op после успешного rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0644, а не 0600: каталог библиотеки читают руками с VPS, и файл,
	// который видит только владелец контейнера, там неудобен.
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// fileNameFor делает из имени торрента имя файла.
//
// Имя приходит из недоверенного .torrent, поэтому режется до одного сегмента
// пути: разделители, управляющие символы и точки по краям убираются, иначе
// загрузка могла бы записать файл куда угодно или спрятать его точкой в начале.
func fileNameFor(name, id string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteRune('_')
		case unicode.IsControl(r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.Trim(strings.TrimSpace(b.String()), ". ")
	if clean == "" || clean == "." || clean == ".." {
		clean = id
	}
	// Предел на длину — не эстетика: ext4 не примет имя длиннее 255 байт,
	// а имя раздачи бывает и длиннее.
	const maxBytes = 180
	if len(clean) > maxBytes {
		clean = strings.ToValidUTF8(clean[:maxBytes], "")
	}
	return clean + ".torrent"
}
