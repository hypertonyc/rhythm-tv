// Package reclaim следит за свободным местом под скачанное и выселяет старое.
//
// Хранилище торрентов ничем не ограничено: каждый добавленный сериал прибавляет
// к нему столько, сколько с него посмотрели, а удаляли это раньше только руками.
// Кончившееся место — не абстракция: рядом на том же диске ffmpeg пишет сегменты
// (0.8–1.2 ГБ на серию, в режиме copy до 6), и ENOSPC случается ПОСРЕДИ СЕРИИ —
// процесс умирает, сеанс уходит в error, зритель остаётся на 25-й минуте.
//
// Поэтому смотреть надо на свободное место ФАЙЛОВОЙ СИСТЕМЫ, а не на размер
// хранилища: занимают его не только торренты, и предел, посчитанный по одному
// хранилищу, ничего не гарантирует.
//
// Порядок выселения — по последнему просмотру, сначала старое (см. Journal).
// Просмотр, а не скачивание: досмотренный два месяца назад сезон занимает
// столько же, сколько нужный сегодня, и терять надо именно его.
//
// Выселение НЕ разрушительно: снятые куски докачаются по требованию, как любые
// недостающие. Цена ошибки — время закачки, а не потерянный вечер. Из этого
// растёт и скупость защит: бережётся ровно то, что сейчас перекодируется.
package reclaim

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/avdav/torrent-media/server/internal/library"
)

// Catalog — что лежит в библиотеке. Реализуется *library.Library.
type Catalog interface {
	StoredFiles() ([]library.StoredFile, error)
}

// Store — хранилище кусков. Реализуется *mediasource.Client.
//
// DropFile обязан не только удалить файл, но и снять отметки готовности
// с его кусков: файл, удалённый в обход отметок, превращается в фантом
// из mediasource/phantom.go — торрент считает его скачанным, читатель
// мгновенно отдаёт нули, и ни одной ошибки в логе не появляется.
type Store interface {
	DropFile(torrentPath string, index int) (int64, error)
}

const (
	// defaultInterval — как часто проверяется место. Раз в минуту: сеанс
	// в режиме copy пишет сегменты быстрее реального времени, но не настолько,
	// чтобы за минуту съесть запас в гигабайты.
	defaultInterval = time.Minute

	// defaultMinFileBytes — ниже этого файл не выселяется. Пак субтитров,
	// .nfo и превью занимают считанные килобайты: удалить их — значит
	// потерять запись в журнале и не освободить ничего.
	defaultMinFileBytes = 16 << 20
)

// Options — всё, что нужно чистке.
type Options struct {
	// StoreDir — каталог хранилища. Свободное место меряется по нему.
	StoreDir string
	// MinFree — порог: ниже этого начинается выселение. Ноль ВЫКЛЮЧАЕТ чистку
	// целиком, оставляя только показания свободного места для страницы.
	MinFree int64
	// TargetFree — до скольких свободных байт чистить. Отдельная величина,
	// а не тот же порог: иначе каждая докачанная серия снова опускала бы
	// свободное место под порог, и чистка шла бы почти непрерывно, по одной
	// серии за раз.
	TargetFree int64
	// Interval — как часто проверяется место. Ноль — defaultInterval.
	Interval time.Duration
	// MinFileBytes — ниже этого файл не выселяется. Ноль — defaultMinFileBytes.
	MinFileBytes int64
	// JournalPath — где лежит журнал просмотров.
	JournalPath string

	Catalog Catalog
	Store   Store

	// Playing отдаёт путь файла, который сейчас перекодируется (относительно
	// хранилища), или пустую строку. Выселять его нельзя: ffmpeg читает его
	// через /raw, и вынутые из-под него куски — это пауза посреди серии,
	// пока они не приедут заново.
	Playing func() string

	// Now и Free подменяются в тестах; иначе time.Now и statfs.
	Now  func() time.Time
	Free func(dir string) (int64, error)
}

// Keeper — чистка: журнал просмотров плюс проход по кандидатам.
type Keeper struct {
	opts    Options
	journal *Journal

	mu            sync.Mutex
	evicted       int
	evictedBytes  int64
	lastEvictedAt *int64
	// complained помнит последнюю жалобу каждого рода. Проход идёт раз
	// в минуту, и постоянная поломка (нет каталога, не читается библиотека)
	// иначе писала бы полторы тысячи одинаковых строк в сутки, вытесняя
	// из лога всё остальное.
	complained map[string]string
}

// Snapshot — место под скачанное глазами страницы библиотеки.
//
// Уезжает в /api/torrents, а не в /api/status: последний сверяется
// с Node-эталоном побайтово, и лишнее поле в нём сломало бы сверку.
// Телевизору это всё не нужно вовсе.
type Snapshot struct {
	// Free — свободно на файловой системе хранилища; -1, если не измеряется.
	Free int64 `json:"free"`
	// MinFree — порог выселения; 0 означает, что чистка выключена.
	MinFree int64 `json:"minFree"`
	// Evicted и EvictedBytes считаются с запуска процесса.
	Evicted       int    `json:"evicted"`
	EvictedBytes  int64  `json:"evictedBytes"`
	LastEvictedAt *int64 `json:"lastEvictedAt"`
}

// New собирает чистку и поднимает журнал просмотров с диска.
func New(opts Options) *Keeper {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.MinFileBytes <= 0 {
		opts.MinFileBytes = defaultMinFileBytes
	}
	if opts.Free == nil {
		opts.Free = freeBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Цель ниже порога — это чистка на каждом тике по одному файлу.
	// Молча выправляем: неверная настройка не должна означать вечное
	// перемалывание хранилища.
	if opts.TargetFree < opts.MinFree {
		opts.TargetFree = opts.MinFree
	}
	return &Keeper{
		opts:       opts,
		journal:    LoadJournal(opts.JournalPath),
		complained: make(map[string]string),
	}
}

// Touch отмечает, что файл сейчас смотрят. Зовётся из /api/start.
func (k *Keeper) Touch(rel string) {
	k.journal.Touch(rel, k.opts.Now())
}

// Enabled сообщает, выселяет ли чистка хоть что-нибудь.
func (k *Keeper) Enabled() bool { return k.opts.MinFree > 0 }

// Run гоняет проверку, пока жив контекст.
//
// Первый проход — через интервал, а не сразу: при старте торрент ещё
// проверяется по хэшам и лечится от фантомов (mediasource), и выдёргивать
// файлы из-под этой проверки незачем. Место за минуту никуда не денется.
func (k *Keeper) Run(ctx context.Context) {
	if !k.Enabled() {
		log.Printf("чистка места выключена (STORE_MIN_FREE_GB=0), журнал просмотров: %d записей",
			k.journal.Len())
		return
	}
	log.Printf("чистка места: порог %s, чистим до %s, проверка раз в %s, журнал просмотров: %d записей",
		humanBytes(k.opts.MinFree), humanBytes(k.opts.TargetFree),
		k.opts.Interval, k.journal.Len())

	ticker := time.NewTicker(k.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.Sweep()
		}
	}
}

// Snapshot — текущее состояние для страницы библиотеки.
func (k *Keeper) Snapshot() Snapshot {
	k.mu.Lock()
	snap := Snapshot{
		MinFree:       k.opts.MinFree,
		Evicted:       k.evicted,
		EvictedBytes:  k.evictedBytes,
		LastEvictedAt: k.lastEvictedAt,
	}
	k.mu.Unlock()

	free, err := k.opts.Free(k.opts.StoreDir)
	if err != nil {
		// -1, а не 0: ноль свободного места — это законное показание,
		// и спутать его с «не смогли измерить» страница не должна.
		snap.Free = -1
		return snap
	}
	snap.Free = free
	return snap
}

// Sweep — один проход: посмотреть на место и, если его мало, выселять
// по одному файлу, пока не станет достаточно.
//
// Возвращает, сколько байт освободил, — это нужно тестам и логу.
func (k *Keeper) Sweep() int64 {
	if !k.Enabled() || k.opts.Catalog == nil || k.opts.Store == nil {
		return 0
	}
	free, err := k.opts.Free(k.opts.StoreDir)
	k.complain("место", err, "чистка места: %s: %v", k.opts.StoreDir, err)
	if err != nil {
		return 0
	}
	if free >= k.opts.MinFree {
		return 0
	}

	files, err := k.opts.Catalog.StoredFiles()
	k.complain("библиотека", err, "чистка места: библиотека недоступна: %v", err)
	if err != nil {
		return 0
	}
	playing := ""
	if k.opts.Playing != nil {
		playing = k.opts.Playing()
	}
	cands := k.candidates(files, playing)
	need := k.opts.TargetFree - free
	var holding int64
	for _, c := range cands {
		holding += c.allocated
	}
	// Сколько кандидаты держат В СУММЕ — единственный способ увидеть в логе,
	// что выселять больше нечего: место кончилось не из-за торрентов,
	// и следующая строка про «выселено 0» тогда не загадка, а ответ.
	log.Printf("чистка места: свободно %s (порог %s), надо освободить %s, кандидатов %d на %s",
		humanBytes(free), humanBytes(k.opts.MinFree), humanBytes(need), len(cands), humanBytes(holding))

	var freed int64
	var evicted int
	for _, c := range cands {
		if freed >= need {
			break
		}
		n, err := k.opts.Store.DropFile(c.file.TorrentFile, c.file.Index)
		if err != nil {
			// Один неудавшийся файл не повод бросать проход: место кончается
			// прямо сейчас, а причина отказа может быть частной (файл держат,
			// .torrent исчез).
			log.Printf("чистка места: %q не выселен: %v", c.file.Rel, err)
			continue
		}
		freed += n
		evicted++
		log.Printf("чистка места: выселено %q — %s, последний просмотр %s",
			c.file.Rel, humanBytes(n), k.describeSeen(c))
	}

	if evicted > 0 {
		now := k.opts.Now().UnixMilli()
		k.mu.Lock()
		k.evicted += evicted
		k.evictedBytes += freed
		k.lastEvictedAt = &now
		k.mu.Unlock()
	}

	after, err := k.opts.Free(k.opts.StoreDir)
	if err != nil {
		after = free + freed
	}
	log.Printf("чистка места: выселено файлов %d на %s, свободно стало %s",
		evicted, humanBytes(freed), humanBytes(after))
	return freed
}

// complain пишет об ошибке одного рода только тогда, когда она меняется,
// и молчит, пока ничего не изменилось. Нулевая ошибка снимает жалобу —
// значит следующая поломка того же рода снова попадёт в лог.
func (k *Keeper) complain(kind string, err error, format string, args ...any) {
	text := ""
	if err != nil {
		text = err.Error()
	}
	k.mu.Lock()
	same := k.complained[kind] == text
	k.complained[kind] = text
	k.mu.Unlock()
	if err != nil && !same {
		log.Printf(format, args...)
	}
}

// candidate — файл, который можно выселить, и всё, что нужно для порядка.
type candidate struct {
	file library.StoredFile
	// allocated — сколько файл занимает НА ДИСКЕ; столько и освободится.
	allocated int64
	// seen — последний просмотр в миллисекундах или, если его не было,
	// mtime файла.
	seen int64
	// watched — отметка о просмотре нашлась в журнале.
	watched bool
}

// candidates отбирает и упорядочивает всё, что можно выселить.
func (k *Keeper) candidates(files []library.StoredFile, playing string) []candidate {
	out := make([]candidate, 0, len(files))
	for _, f := range files {
		if f.Rel == playing {
			continue
		}
		full := filepath.Join(k.opts.StoreDir, filepath.FromSlash(f.Rel))
		allocated, mod, err := fileUsage(full)
		if err != nil {
			// Файла нет вовсе — обычное дело: скачивают по требованию,
			// и большая часть библиотеки на диске не лежит.
			continue
		}
		if allocated < k.opts.MinFileBytes {
			continue
		}
		c := candidate{file: f, allocated: allocated, seen: mod.UnixMilli()}
		// Просмотр важнее mtime, но mtime — честный запасной вариант:
		// файл, скачанный вчера и ещё не открытый, не должен вылетать
		// раньше серии, просмотренной год назад.
		if at, ok := k.journal.At(f.Rel); ok {
			c.seen, c.watched = at, true
		}
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].seen != out[j].seen {
			return out[i].seen < out[j].seen
		}
		// Путь вторым ключом — ради предсказуемости: у файлов одного пака
		// mtime сплошь и рядом совпадает до миллисекунды.
		return out[i].file.Rel < out[j].file.Rel
	})
	return out
}

// describeSeen печатает, откуда взялся порядок, — по журналу или по mtime.
func (k *Keeper) describeSeen(c candidate) string {
	when := time.UnixMilli(c.seen).Format("2006-01-02 15:04")
	if !c.watched {
		return when + " (не смотрели, время файла)"
	}
	return when
}

// humanBytes печатает размер так же, как это делает mediasource в своих логах:
// логи чистки и логи торрента читают подряд, в одном docker logs.
func humanBytes(n int64) string {
	const unit = 1024
	if n > -unit && n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit || x <= -unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
