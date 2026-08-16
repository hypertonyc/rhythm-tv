package mediasource

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
)

// Anacrolix — единственное место в проекте, которое знает про anacrolix/torrent.
type Anacrolix struct {
	client       *torrent.Client
	dataDir      string
	persistStore bool

	// ready снимается один раз, когда приедут метаданные. До этого /api/files
	// и /api/probe обязаны отдавать 503, как это делал Node с torrent === null.
	ready atomic.Bool
	tor   atomic.Pointer[torrent.Torrent]

	readahead int64
	meter     *Meter
	stopOnce  sync.Once
	stop      chan struct{}
	sampler   sync.WaitGroup
}

// Options — настройки источника.
type Options struct {
	TorrentPath string
	DataDir     string
	// Readahead задаёт окно упреждающего чтения на Reader. Ноль — оставить
	// адаптивное поведение anacrolix.
	Readahead int64
	// Seed включает раздачу. По умолчанию выключено, как и у webtorrent
	// в этом сценарии: сервер здесь потребитель, а не сидбокс.
	Seed bool
	// ListenPort — порт для входящих пиров. Ноль отдаёт выбор библиотеке.
	ListenPort int
	// VerifyOnStart прогоняет хэш-проверку уже лежащих на диске данных.
	//
	// Нужно ровно один раз — при переезде с Node. Раскладка файлов у webtorrent
	// и anacrolix совпадает (<хранилище>/<имя торрента>/<путь>), но база
	// готовности кусков у anacrolix своя и пустая, поэтому без проверки он
	// счёл бы все 27 ГБ отсутствующими и начал качать заново.
	VerifyOnStart bool
	// PersistStore оставляет скачанное на диске после остановки.
	//
	// Node стирал хранилище всегда (destroyStoreOnDestroy: true), и порт это
	// сначала воспроизводил. На проде так нельзя: там уже лежат десятки
	// гигабайт, и каждый деплой означал бы повторную закачку сериала целиком.
	// Контракта с телевизором это не касается вовсе — поведение при выключении
	// снаружи не наблюдаемо.
	PersistStore bool
}

const defaultReadahead = 8 << 20

// NewAnacrolix поднимает клиент и добавляет торрент.
//
// Возвращается сразу, не дожидаясь метаданных: Node вёл себя так же, а телевизор
// на это рассчитывает — он показывает «Torrent metadata loading…» по ответу
// /api/status и повторяет опрос.
func NewAnacrolix(opts Options) (*Anacrolix, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = opts.DataDir
	cfg.Seed = opts.Seed
	cfg.Logger = quietLogger()
	cfg.Slogger = quietSlogger()
	if opts.ListenPort != 0 {
		cfg.ListenPort = opts.ListenPort
	}

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("torrent client: %w", err)
	}

	a := &Anacrolix{
		client:       client,
		dataDir:      opts.DataDir,
		persistStore: opts.PersistStore,
		meter:        NewMeter(5, func() int64 { return time.Now().UnixMilli() }),
		stop:         make(chan struct{}),
	}
	if opts.Readahead == 0 {
		opts.Readahead = defaultReadahead
	}
	a.readahead = opts.Readahead

	t, err := client.AddTorrentFromFile(opts.TorrentPath)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("add torrent: %w", err)
	}

	go func() {
		<-t.GotInfo()

		if opts.VerifyOnStart {
			// Проверка идёт ДО снятия флага готовности: иначе первое же чтение
			// решило бы, что кусков нет, и потянуло бы их из сети заново.
			// Клиент это переживает — он опрашивает /api/status и показывает
			// «Torrent metadata loading…».
			log.Printf("verifying existing data in %s, this can take a few minutes", opts.DataDir)
			started := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			go func() { <-a.stop; cancel() }()
			if err := t.VerifyDataContext(ctx); err != nil {
				log.Printf("verify failed: %v (продолжаем; недостающее докачается)", err)
			} else {
				log.Printf("verified in %s: %s already on disk",
					time.Since(started).Round(time.Second), humanBytes(t.BytesCompleted()))
			}
			cancel()
		}

		// DownloadAll() НЕ вызывается сознательно. У anacrolix куски по
		// умолчанию имеют приоритет None, и это ровно то, что в webtorrent
		// давал deselect:true: данные тянет только живой Reader.
		// Явная простановка ниже — страховка на случай смены умолчания.
		for _, f := range t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		a.tor.Store(t)
		a.ready.Store(true)
		log.Printf("torrent ready: %q, %d files", t.Name(), len(t.Files()))
	}()

	a.sampler.Add(1)
	go a.sampleRate()

	return a, nil
}

// sampleRate питает измеритель скорости.
//
// В webtorrent throughput кормился на каждом пришедшем блоке; здесь — опросом
// раз в 100 мс, ровно в такт разрешению самого измерителя, так что в сумме
// получается то же самое. Считаем по BytesReadUsefulData: это «пришедшее по
// делу», ближайший аналог того, что скармливал webtorrent.
func (a *Anacrolix) sampleRate() {
	defer a.sampler.Done()
	ticker := time.NewTicker(meterTimeDiff * time.Millisecond)
	defer ticker.Stop()

	var prev int64
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			t := a.tor.Load()
			if t == nil {
				continue
			}
			stats := t.Stats()
			cur := stats.BytesReadUsefulData.Int64()
			if delta := cur - prev; delta > 0 {
				a.meter.Add(float64(delta))
			}
			prev = cur
		}
	}
}

func (a *Anacrolix) Ready() bool { return a.ready.Load() }

func (a *Anacrolix) Name() string {
	if t := a.tor.Load(); t != nil {
		return t.Name()
	}
	return ""
}

// Files отдаёт файлы в порядке метаинформации. Порядок несущий: телевизор
// хранит позиции просмотра по индексу в этом списке.
func (a *Anacrolix) Files() []File {
	t := a.tor.Load()
	if t == nil {
		return nil
	}
	all := t.Files()
	files := make([]File, 0, len(all))
	for i, f := range all {
		// webtorrent-овский file.name — это последний сегмент пути,
		// а не путь целиком (проверено сверкой /api/files с эталоном).
		files = append(files, File{Index: i, Name: path.Base(f.DisplayPath()), Length: f.Length()})
	}
	return files
}

func (a *Anacrolix) Open(index int) (Reader, error) {
	t := a.tor.Load()
	if t == nil {
		return nil, ErrNotReady
	}
	all := t.Files()
	if index < 0 || index >= len(all) {
		return nil, os.ErrNotExist
	}
	r := all[index].NewReader()
	r.SetReadahead(a.readahead)
	return r, nil
}

func (a *Anacrolix) Stats() Stats {
	t := a.tor.Load()
	if t == nil {
		return Stats{}
	}
	downloaded := t.BytesCompleted()
	length := t.Length()
	// Защита от деления на ноль обязательна: NaN в JSON это не null, как в JS,
	// а ошибка маршалинга, то есть 500 на /api/status.
	var progress float64
	if length > 0 {
		progress = float64(downloaded) / float64(length)
	}
	return Stats{
		// ActivePeers, а не TotalPeers: последний считает и тех, о ком мы
		// только знаем, и клиент показывал бы пиров там, где их нет.
		Peers:         t.Stats().ActivePeers,
		DownloadSpeed: a.meter.Rate(),
		Downloaded:    downloaded,
		Progress:      progress,
	}
}

// Close останавливает клиент и, если не велено иначе, стирает хранилище.
//
// Стирание — поведение Node (destroyStoreOnDestroy: true), и по умолчанию оно
// сохранено. Но на проде, где уже лежат десятки гигабайт, каждый деплой означал
// бы повторную закачку сериала целиком, поэтому есть TORRENT_STORE_PERSIST=1.
// Наружу это не наблюдаемо: контракта с телевизором касается только то,
// что сервер отвечает, а не то, что он делает при выключении.
func (a *Anacrolix) Close() error {
	a.stopOnce.Do(func() { close(a.stop) })
	a.sampler.Wait()
	if a.client != nil {
		a.client.Close()
	}
	if a.dataDir != "" && !a.persistStore {
		os.RemoveAll(a.dataDir)
	}
	return nil
}

// ctxReader адаптирует Reader к io.Reader, привязывая чтение к контексту.
// Нужен там, где стандартная библиотека хочет обычный io.Reader (io.CopyN).
type ctxReader struct {
	ctx context.Context
	r   Reader
}

func (c ctxReader) Read(p []byte) (int, error) { return c.r.ReadContext(c.ctx, p) }

// WithContext оборачивает Reader в io.Reader, отменяемый контекстом.
func WithContext(ctx context.Context, r Reader) io.Reader { return ctxReader{ctx: ctx, r: r} }

// humanBytes печатает размер так, чтобы мелкие значения не превращались в «0.0 GB».
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
