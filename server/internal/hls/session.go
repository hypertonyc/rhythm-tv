package hls

import (
	"os/exec"
	"sync"
	"time"
)

// Фазы сеанса. Двигаются только вперёд: preparing → buffering (есть первый
// сегмент) → ready (есть два) → finished (ffmpeg вышел сам).
const (
	phasePreparing = "preparing"
	phaseBuffering = "buffering"
	phaseReady     = "ready"
	phaseFinished  = "finished"
)

// Причины остановки. Живут ОТДЕЛЬНО от фазы: иначе остановка затирала бы фазу,
// и «остановлен, но два сегмента готовы» стало бы неотличимо от «ещё готовится».
const (
	reasonStopped  = "stopped"
	reasonReplaced = "replaced"
)

// Session — один запуск ffmpeg.
//
// ВСЕ изменяемые поля защищены Manager.mu, а не собственным мьютексом сеанса.
// Один лок на всё убирает порядок блокировок как класс, а нагрузка тут —
// тик раз в 500 мс плюс пара операций в секунду.
type Session struct {
	id   string
	dir  string
	name string

	index                int
	audio, sub           string
	videoMode, audioMode string
	start                float64

	downloadedAtStart int64
	startedAt         int64

	phase        string
	stopReason   string
	errMsg       *string
	exitCode     *int
	ffmpegPid    *int
	exited       bool
	segments     int
	bytesOut     int64
	lastOutputAt *int64
	// nextSeq — номер следующего ожидаемого сегмента. nil означает «ещё не знаем,
	// с какого номера ffmpeg начал»; см. pollSegments.
	nextSeq *int

	// pipeline — из чего во что идёт перекодирование. Считается один раз
	// при старте, теми же данными, что решали вопрос о копировании.
	pipeline Pipeline

	cmd    *exec.Cmd
	stderr *ringWriter
	// progress — машинный отчёт ffmpeg о ходе работы. nil у подобранного
	// сеанса: своего процесса у него нет, а к чужому stdout не подключиться.
	progress *progressWriter

	stopMonitor chan struct{}
	monitorDone chan struct{}
	stopOnce    sync.Once

	killTimer    *time.Timer
	cleanupTimer *time.Timer
}

// Snapshot — то, что уходит в /api/start, /api/hls-status и в поле playback
// у /api/status.
//
// Порядок полей повторяет литерал sessionSnapshot() из server.mjs:385-409.
// Указатели там, где Node отдавал null: клиент отличает null от отсутствия ключа.
type Snapshot struct {
	ID        string  `json:"id"`
	Index     int     `json:"index"`
	Name      string  `json:"name"`
	Audio     string  `json:"audio"`
	Sub       string  `json:"sub"`
	VideoMode string  `json:"videoMode"`
	AudioMode string  `json:"audioMode"`
	Start     float64 `json:"start"`
	State     string  `json:"state"`
	StartedAt int64   `json:"startedAt"`
	Segments  int     `json:"segments"`
	BytesOut  int64   `json:"bytesOut"`

	LastOutputAt *int64  `json:"lastOutputAt"`
	FFmpegPID    *int    `json:"ffmpegPid"`
	ExitCode     *int    `json:"exitCode"`
	Error        *string `json:"error"`

	// Playlist обязан оставаться корневым относительным путём: снаружи сервер
	// стоит за reverse-proxy, который срезает из пути секретный /<token>,
	// а клиент приклеивает свой serverBase обратно. Абсолютный URL сломал бы это.
	Playlist         string  `json:"playlist"`
	SubtitlePlaylist *string `json:"subtitlePlaylist"`
	Format           string  `json:"format"`

	DownloadedSinceStart int64 `json:"downloadedSinceStart"`
}

// Progress — ответ /api/pipeline: чем сеанс занят прямо сейчас и из чего
// во что перекодирует.
//
// Отдельный тип, а не поля в Snapshot, и это не вкус: Snapshot уходит в поле
// playback у /api/status, а тот сверяется с Node-эталоном по форме — лишний
// ключ там ломает сверку. Заодно граница честная: /api/pipeline эталона
// не имеет вовсе, поэтому его формат можно менять свободно.
//
// EncodedMs, Speed и EtaMs — указатели, потому что «не измерено» и ноль здесь
// разные вещи: ноль в скорости означает «ffmpeg жив и не производит ничего»,
// то есть симптом, а отсутствие отчёта означает, что он ещё не начинал.
type Progress struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Index    int     `json:"index"`
	State    string  `json:"state"`
	Start    float64 `json:"start"`
	Segments int     `json:"segments"`

	// StartupSegments и StartupTargetMs — то, чего ждёт клиент перед первой
	// картинкой: два сегмента, то есть 8 секунд видео. Цифры уезжают наружу,
	// а не зашиваются в клиента, чтобы правка -hls_time не сделала прогресс
	// на экране враньём.
	StartupSegments int   `json:"startupSegments"`
	StartupTargetMs int64 `json:"startupTargetMs"`

	EncodedMs *int64   `json:"encodedMs"`
	Speed     *float64 `json:"speed"`
	EtaMs     *int64   `json:"etaMs"`

	DownloadedSinceStart int64    `json:"downloadedSinceStart"`
	Pipeline             Pipeline `json:"pipeline"`
}

// state сводит фазу, причину остановки и ошибку в одно поле:
// ошибка важнее остановки, остановка важнее фазы.
func (s *Session) state() string {
	if s.errMsg != nil {
		return "error"
	}
	if s.stopReason != "" {
		return s.stopReason
	}
	return s.phase
}

// advancePhase двигает фазу только вперёд и никогда не трогает finished.
func (s *Session) advancePhase() {
	if s.phase == phaseFinished {
		return
	}
	switch {
	case s.segments >= StartupSegments:
		s.phase = phaseReady
	case s.segments >= 1:
		s.phase = phaseBuffering
	}
}

// ringWriter хранит хвост stderr ffmpeg.
//
// Отдельный тип, а не bytes.Buffer, потому что ffmpeg на битом входе способен
// выдать сотни мегабайт предупреждений, а нужны только последние строки.
// Лока нет намеренно: exec владеет копировщиком, cmd.Wait() его джойнит,
// и читатель у буфера ровно один — та же горутина, уже после Wait.
type ringWriter struct {
	limit int
	buf   []byte
}

func (w *ringWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	// Держим запас в байтах, чтобы не резать по середине руны; окончательная
	// обрезка до нужного числа символов делается в String().
	if max := w.limit * 4; len(w.buf) > max {
		w.buf = w.buf[len(w.buf)-max:]
	}
	return len(p), nil
}

// String отдаёт последние limit символов.
func (w *ringWriter) String() string {
	r := []rune(string(w.buf))
	if len(r) > w.limit {
		r = r[len(r)-w.limit:]
	}
	return string(r)
}

// tailRunes — аналог .slice(-n) для строки.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return string(r)
}
