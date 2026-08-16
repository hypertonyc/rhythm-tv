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

// Client — единственное место в проекте, которое знает про anacrolix/torrent.
//
// Клиент один на процесс, а торрентов через него проходит много: библиотека
// переключает активный, добавляя новый и роняя прежний. Отдельный клиент
// на каждый торрент завести нельзя — ListenPort фиксирован (TORRENT_PORT),
// и второй клиент не занял бы порт, пока первый жив.
type Client struct {
	client       *torrent.Client
	dataDir      string
	persistStore bool
	readahead    int64
	verify       bool
	warmupBytes  int64
}

// Options — настройки клиента.
type Options struct {
	DataDir string
	// Readahead задаёт окно упреждающего чтения на Reader. Ноль — оставить
	// адаптивное поведение anacrolix.
	Readahead int64
	// Seed включает раздачу. По умолчанию выключено, как и у webtorrent
	// в этом сценарии: сервер здесь потребитель, а не сидбокс.
	Seed bool
	// ListenPort — порт для входящих пиров. Ноль отдаёт выбор библиотеке.
	ListenPort int
	// VerifyOnStart прогоняет хэш-проверку уже лежащих на диске данных
	// при добавлении каждого торрента.
	//
	// Нужно ровно один раз — при переезде с Node: раскладка файлов у webtorrent
	// и anacrolix совпадает (<хранилище>/<имя торрента>/<путь>), но база
	// готовности кусков у anacrolix своя и пустая, поэтому без проверки он
	// счёл бы все 27 ГБ отсутствующими и начал качать заново.
	VerifyOnStart bool
	// WarmupBytes — сколько байт прочитать при старте, чтобы поднять рой.
	// Ноль отключает разогрев. См. warmSwarm.
	WarmupBytes int64
	// PersistStore оставляет скачанное на диске после остановки.
	//
	// Node стирал хранилище всегда (destroyStoreOnDestroy: true), и порт это
	// сначала воспроизводил. На проде так нельзя: там уже лежат десятки
	// гигабайт, и каждый деплой означал бы повторную закачку сериала целиком.
	// Контракта с телевизором это не касается вовсе — поведение при выключении
	// снаружи не наблюдаемо.
	//
	// Смена активного торрента хранилище не трогает НИКОГДА: каталог общий
	// на все торренты библиотеки, и стирать его при переключении значило бы
	// выбрасывать чужие гигабайты.
	PersistStore bool
}

const defaultReadahead = 8 << 20

// NewClient поднимает клиент anacrolix. Торренты добавляются отдельно — Add.
func NewClient(opts Options) (*Client, error) {
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

	if opts.Readahead == 0 {
		opts.Readahead = defaultReadahead
	}
	return &Client{
		client:       client,
		dataDir:      opts.DataDir,
		persistStore: opts.PersistStore,
		readahead:    opts.Readahead,
		verify:       opts.VerifyOnStart,
		warmupBytes:  opts.WarmupBytes,
	}, nil
}

// Add добавляет торрент и отдаёт его как Source.
//
// Возвращается сразу, не дожидаясь метаданных: Node вёл себя так же, а телевизор
// на это рассчитывает — он показывает «Torrent metadata loading…» по ответу
// /api/status и повторяет опрос.
func (c *Client) Add(torrentPath string) (*Torrent, error) {
	t, err := c.client.AddTorrentFromFile(torrentPath)
	if err != nil {
		return nil, fmt.Errorf("add torrent: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tr := &Torrent{
		raw:       t,
		readahead: c.readahead,
		meter:     NewMeter(5, func() int64 { return time.Now().UnixMilli() }),
		stop:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}

	go func() {
		select {
		case <-t.GotInfo():
		case <-tr.stop:
			// Торрент сняли раньше, чем приехали метаданные: без этой ветки
			// горутина висела бы до конца процесса на каждом переключении.
			return
		}

		if c.verify {
			// Проверка идёт ДО снятия флага готовности: иначе первое же чтение
			// решило бы, что кусков нет, и потянуло бы их из сети заново.
			// Клиент это переживает — он опрашивает /api/status и показывает
			// «Torrent metadata loading…».
			log.Printf("verifying existing data in %s, this can take a few minutes", c.dataDir)
			started := time.Now()
			if err := t.VerifyDataContext(ctx); err != nil {
				log.Printf("verify failed: %v (продолжаем; недостающее докачается)", err)
			} else {
				log.Printf("verified in %s: %s already on disk",
					time.Since(started).Round(time.Second), humanBytes(t.BytesCompleted()))
			}
		}

		// Лечение фантомных файлов — тоже ДО снятия флага готовности:
		// иначе телевизор успеет запустить перекодирование того файла,
		// который сейчас перепроверяется, и получит нули. Полная проверка
		// выше (c.verify) делает то же самое и надёжнее, но она стоит минут
		// и включается руками, а эта — один stat на файл. См. phantom.go.
		if healed := healPhantomFiles(ctx, t, c.dataDir); healed > 0 {
			log.Printf("phantom files: вылечено %d — недостающее докачается по требованию", healed)
		}

		// DownloadAll() НЕ вызывается сознательно. У anacrolix куски по
		// умолчанию имеют приоритет None, и это ровно то, что в webtorrent
		// давал deselect:true: данные тянет только живой Reader.
		// Явная простановка ниже — страховка на случай смены умолчания.
		for _, f := range t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		tr.tor.Store(t)

		// Разогрев ДО снятия флага готовности, а не после. Иначе сервер
		// объявляет себя готовым, ещё не имея пиров, телевизор сразу лезет
		// с /api/probe и /api/start — и получает «ffprobe timed out» или
		// сеанс, умерший в state=error. Пока флага нет, клиент показывает
		// «Torrent metadata loading…» и спокойно опрашивает дальше.
		//
		// Уже начатый до выкатки просмотр это не задевает: его сегменты
		// лежат на диске, а подобранный сеанс отдаётся без участия торрента.
		if c.warmupBytes > 0 {
			tr.warmSwarm(t, c.warmupBytes)
		}

		tr.ready.Store(true)
		log.Printf("torrent ready: %q, %d files", t.Name(), len(t.Files()))

		// И дальше — на ходу: файл теряет данные во время просмотра, а не
		// при старте, так что разовой проверки выше мало. См. phantom.go.
		go watchPhantomFiles(ctx, t, c.dataDir)
	}()

	tr.sampler.Add(1)
	go tr.sampleRate()

	return tr, nil
}

// Close останавливает клиент и, если не велено иначе, стирает хранилище.
//
// Стирание — поведение Node (destroyStoreOnDestroy: true), и по умолчанию оно
// сохранено. Но на проде, где уже лежат десятки гигабайт, каждый деплой означал
// бы повторную закачку сериала целиком, поэтому есть TORRENT_STORE_PERSIST=1.
// Наружу это не наблюдаемо: контракта с телевизором касается только то,
// что сервер отвечает, а не то, что он делает при выключении.
func (c *Client) Close() error {
	if c.client != nil {
		c.client.Close()
	}
	if c.dataDir != "" && !c.persistStore {
		os.RemoveAll(c.dataDir)
	}
	return nil
}

// Torrent — один торрент клиента, он же Source для HTTP-слоя.
type Torrent struct {
	raw *torrent.Torrent

	// ready снимается один раз, когда приедут метаданные. До этого /api/files
	// и /api/probe обязаны отдавать 503, как это делал Node с torrent === null.
	ready atomic.Bool
	tor   atomic.Pointer[torrent.Torrent]

	readahead int64
	meter     *Meter
	stopOnce  sync.Once
	stop      chan struct{}
	sampler   sync.WaitGroup

	// ctx закрывается вместе с торрентом и подмешивается в каждое чтение.
	// Без него читатель, стоящий на раздаче без пиров, пережил бы снятие
	// торрента и висел бы до конца процесса — а вместе с ним и окно
	// приоритета вокруг своей позиции.
	ctx    context.Context
	cancel context.CancelFunc
}

// sampleRate питает измеритель скорости.
//
// В webtorrent throughput кормился на каждом пришедшем блоке; здесь — опросом
// раз в 100 мс, ровно в такт разрешению самого измерителя, так что в сумме
// получается то же самое. Считаем по BytesReadUsefulData: это «пришедшее по
// делу», ближайший аналог того, что скармливал webtorrent.
func (t *Torrent) sampleRate() {
	defer t.sampler.Done()
	ticker := time.NewTicker(meterTimeDiff * time.Millisecond)
	defer ticker.Stop()

	var prev int64
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			tt := t.tor.Load()
			if tt == nil {
				continue
			}
			stats := tt.Stats()
			cur := stats.BytesReadUsefulData.Int64()
			if delta := cur - prev; delta > 0 {
				t.meter.Add(float64(delta))
			}
			prev = cur
		}
	}
}

func (t *Torrent) Ready() bool { return t.ready.Load() }

func (t *Torrent) Name() string {
	if tt := t.tor.Load(); tt != nil {
		return tt.Name()
	}
	return ""
}

// Files отдаёт файлы в порядке метаинформации. Порядок несущий: телевизор
// хранит позиции просмотра по индексу в этом списке.
func (t *Torrent) Files() []File {
	tt := t.tor.Load()
	if tt == nil {
		return nil
	}
	all := tt.Files()
	files := make([]File, 0, len(all))
	for i, f := range all {
		// webtorrent-овский file.name — это последний сегмент пути,
		// а не путь целиком (проверено сверкой /api/files с эталоном).
		files = append(files, File{Index: i, Name: path.Base(f.DisplayPath()), Length: f.Length()})
	}
	return files
}

func (t *Torrent) Open(index int) (Reader, error) {
	tt := t.tor.Load()
	if tt == nil {
		return nil, ErrNotReady
	}
	all := tt.Files()
	if index < 0 || index >= len(all) {
		return nil, os.ErrNotExist
	}
	r := all[index].NewReader()
	r.SetReadahead(t.readahead)
	return &boundReader{r: r, owner: t.ctx}, nil
}

func (t *Torrent) Stats() Stats {
	tt := t.tor.Load()
	if tt == nil {
		return Stats{}
	}
	downloaded := tt.BytesCompleted()
	length := tt.Length()
	// Защита от деления на ноль обязательна: NaN в JSON это не null, как в JS,
	// а ошибка маршалинга, то есть 500 на /api/status.
	var progress float64
	if length > 0 {
		progress = float64(downloaded) / float64(length)
	}
	return Stats{
		// ActivePeers, а не TotalPeers: последний считает и тех, о ком мы
		// только знаем, и клиент показывал бы пиров там, где их нет.
		Peers:         tt.Stats().ActivePeers,
		DownloadSpeed: t.meter.Rate(),
		Downloaded:    downloaded,
		Progress:      progress,
	}
}

// Close снимает торрент с клиента. Скачанное на диске остаётся: хранилище
// общее на всю библиотеку, и чистит его только Client.Close.
func (t *Torrent) Close() error {
	t.stopOnce.Do(func() {
		close(t.stop)
		t.cancel()
	})
	t.sampler.Wait()
	if t.raw != nil {
		t.raw.Drop()
	}
	return nil
}

// boundReader привязывает чтение к жизни торрента.
//
// Без этого закрытие торрента не будило бы читателя, стоящего внутри
// ReadContext: у anacrolix Drop не обрывает выданные Reader'ы, и префетч
// снятого торрента продолжал бы держать окно приоритета.
type boundReader struct {
	r     torrent.Reader
	owner context.Context
}

func (b *boundReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	if b.owner != nil {
		merged, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(b.owner, cancel)
		defer stop()
		ctx = merged
	}
	return b.r.ReadContext(ctx, p)
}

func (b *boundReader) Seek(off int64, whence int) (int64, error) { return b.r.Seek(off, whence) }
func (b *boundReader) SetReadahead(n int64)                      { b.r.SetReadahead(n) }
func (b *boundReader) Close() error                              { return b.r.Close() }

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

// warmSwarm читает несколько килобайт, чтобы клиент нашёл пиров заранее.
//
// Пиры подключаются только при спросе: куски имеют приоритет None, и пока нет
// живого Reader, клиент никого не ищет. Из-за этого сразу после перезапуска
// первый же сеанс попадает на пустой рой, чтение через /raw встаёт, и ffmpeg
// выходит — сеанс уходит в state=error, хотя через минуту всё заработало бы.
//
// Читается ПОСЛЕ снятия флага готовности: разогрев не задерживает ответы
// сервера, он лишь сокращает окно. Ошибка не важна — это оптимизация.
func (tr *Torrent) warmSwarm(t *torrent.Torrent, want int64) {
	files := t.Files()
	if len(files) == 0 {
		return
	}
	r := files[0].NewReader()
	defer r.Close()

	started := time.Now()
	n, err := io.CopyN(io.Discard, ctxReader{ctx: tr.ctx, r: r}, want)
	log.Printf("swarm warmup: %s in %s, peers=%d (err=%v)",
		humanBytes(n), time.Since(started).Round(time.Millisecond),
		t.Stats().ActivePeers, err)
}
