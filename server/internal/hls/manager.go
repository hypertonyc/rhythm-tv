package hls

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/subs"
)

const (
	monitorInterval = 500 * time.Millisecond
	killGrace       = 5 * time.Second
	cleanupDelay    = 2 * time.Minute
	cleanupRetry    = 30 * time.Second
	stderrLimit     = 16000
	errorTailLimit  = 3000

	// SegmentSeconds — длина сегмента, `-hls_time`. Константа, а не литерал,
	// потому что по ней же считается, сколько ВИДЕО нужно произвести до старта
	// воспроизведения: разъехавшись, они превратили бы прогресс на экране
	// телевизора в тихое враньё.
	SegmentSeconds = 4
	// StartupSegments — сколько сегментов ждёт клиент, прежде чем открыть
	// плейлист. Та же цифра стоит в фазе ready (см. advancePhase).
	StartupSegments = 2
)

// Manager владеет ВСЕМ изменяемым состоянием сеансов.
//
// Node был безопасен тем, что между mkdir и spawn нет ни одного await, а
// колбэки таймеров и обработчики запросов сериализованы event loop'ом.
// В Go эта сериализация восстанавливается явно: единственный мьютекс mu.
type Manager struct {
	// TmpDir — каталог для tms-hls-*. Обычно os.TempDir().
	TmpDir string
	// FFmpeg — имя бинарника; подменяется в тестах на шим.
	FFmpeg string
	// AllowCopy — HLS_ALLOW_COPY: false заставляет перекодировать всегда.
	AllowCopy bool
	// RawURL отдаёт адрес, по которому ffmpeg читает файл через наш HTTP.
	RawURL func(index int) string
	// Keyframe отдаёт время последнего ключевого кадра не позже start —
	// им перемотка подтягивается на границу GOP, без чего её нельзя копировать
	// (см. media.KeyframeFinder). nil означает «не искать»: сеанс тогда ведёт
	// себя как до 18.08.2026 и перекодирует любую перемотку.
	Keyframe func(index, videoIndex int, start float64) (float64, bool)
	// Downloaded — текущий счётчик скачанного, для downloadedSinceStart.
	Downloaded func() int64
	// NowMilli подменяется в тестах; иначе time.Now().UnixMilli.
	NowMilli func() int64

	mu       sync.Mutex
	sessions map[string]*Session
	// MULTI-TORRENT PIN: при мультиторренте это станет map по id торрента.
	// sessions менять не придётся — id сеансов и так глобально уникальны.
	active *Session
	closed bool
}

// StartOptions — то, что приходит из /api/start.
type StartOptions struct {
	Index     int
	Meta      *media.Result
	AudioPref string
	SubPref   string
	Start     float64
}

// Start запускает новый сеанс, убивая предыдущий.
func (m *Manager) Start(opts StartOptions) (Snapshot, error) {
	meta := opts.Meta
	if meta.Video == nil {
		return Snapshot{}, media.ErrNoVideoStream
	}

	audio := media.ChooseAudio(meta.Audio, opts.AudioPref)
	var subtitle *media.SubtitleTrack
	if opts.SubPref != "off" {
		subtitle = media.ChooseSubtitle(meta.Subtitles, opts.SubPref)
	}

	// Дальше по всему методу идёт seek.Start, а не opts.Start: попросили одну
	// секунду, а сеанс живёт с другой, подтянутой к ключевому кадру. Разница
	// невелика (в среднем половина GOP, на «Друзьях» это ~2.5 с назад),
	// но она обязана быть ОДНОЙ на весь сеанс: на неё сдвигаются метки внешних
	// субтитров, от неё считается -ss, её же телевизор получает в снимке.
	seek := m.alignSeek(opts, meta.Video)

	id := m.newID()
	dir := filepath.Join(m.tmpDir(), "tms-hls-"+id)

	// Каталог создаётся ДО остановки предыдущего сеанса: если mkdir упадёт,
	// работающее воспроизведение не должно оказаться убитым зря.
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return Snapshot{}, err
	}

	// Субтитры из отдельного файла раскладываются в каталог сеанса ДО запуска
	// ffmpeg — он о них ничего не знает (см. BuildArgs). Побочно это выходит
	// лучше встроенных: телевизор получает весь текст серии сразу, не дожидаясь,
	// пока перекодирование доползёт до нужной минуты.
	//
	// Неудача не повод не показывать серию: пропавший или битый файл гасит
	// дорожку и только её.
	if subtitle.External() {
		if err := subs.WriteSession(dir, subtitle.SourcePath, seek.Start, meta.Duration); err != nil {
			log.Printf("HLS [%d] субтитры %s: %v", opts.Index, subtitle.SourcePath, err)
			subtitle = nil
		}
	}

	copyVideo := media.CanCopyVideo(meta.Video, seek, m.AllowCopy)
	copyAudio := audio != nil && media.CanCopyAudio(audio, seek, m.AllowCopy)

	args := BuildArgs(Params{
		RawURL:     m.RawURL(opts.Index),
		Dir:        dir,
		VideoIndex: meta.Video.Index,
		Audio:      audio,
		Subtitle:   subtitle,
		Start:      seek.Start,
		CopyVideo:  copyVideo,
		CopyAudio:  copyAudio,
	})

	videoMode := "transcode"
	if copyVideo {
		videoMode = "copy"
	}
	audioMode := "none"
	if audio != nil {
		audioMode = "transcode"
		if copyAudio {
			audioMode = "copy"
		}
	}

	log.Printf("HLS [%d] audio=%s sub=%s start=%s video=%s audio-mode=%s mode=%s",
		opts.Index, codeOr(audio != nil, audioCode(audio), "none"),
		codeOr(subtitle != nil, subCode(subtitle), "off"),
		jscompat.ToFixed(seek.Start, 1), videoMode, audioMode,
		codeOr(subtitle != nil, codeOr(subtitle.External(), "webvtt-file", "webvtt"), "no-subs"))

	// Дальше — один блок под локом, без единой точки ожидания внутри.
	// Ровно так же устроен оригинал: между mkdir и spawn там нет await.
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("manager is shutting down")
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*Session)
	}
	if m.active != nil {
		m.stopLocked(m.active, reasonReplaced)
		m.scheduleCleanupLocked(m.active, cleanupDelay)
		m.active = nil
	}

	s := &Session{
		id:                id,
		dir:               dir,
		name:              meta.Name,
		index:             opts.Index,
		audio:             codeOr(audio != nil, audioCode(audio), "none"),
		sub:               codeOr(subtitle != nil, subCode(subtitle), "off"),
		videoMode:         videoMode,
		audioMode:         audioMode,
		start:             seek.Start,
		downloadedAtStart: m.downloaded(),
		startedAt:         m.now(),
		phase:             phasePreparing,
		pipeline:          describePipeline(meta.Video, audio, copyVideo, copyAudio),
		stderr:            &ringWriter{limit: stderrLimit},
		progress:          &progressWriter{},
		stopMonitor:       make(chan struct{}),
		monitorDone:       make(chan struct{}),
	}
	m.sessions[id] = s
	m.active = s
	// Пишется до запуска ffmpeg: каталог должен быть опознаваем, даже если
	// процесс не стартует вовсе.
	writeManifest(s)

	cmd := exec.Command(m.ffmpeg(), args...)
	// stderr копится в кольцевом буфере, stdout разбирается на ходу
	// (`-progress pipe:1`). Пайпов не берём ни там, ни там: StdoutPipe
	// и StderrPipe закрываются в Wait, и читать из них после него нельзя,
	// а копировщик, которому отдан io.Writer, exec джойнит сам.
	cmd.Stderr = s.stderr
	cmd.Stdout = s.progress
	if err := cmd.Start(); err != nil {
		// Node в этом случае эмитит и 'error', и 'close' с code === null.
		// Без этой ветки монитор вечно тикал бы по каталогу, в который
		// никто не пишет.
		msg := err.Error()
		s.errMsg = &msg
		s.exited = true
		s.stopOnce.Do(func() { close(s.stopMonitor) })
		close(s.monitorDone)
		return m.snapshotLocked(s), nil
	}
	s.cmd = cmd
	pid := cmd.Process.Pid
	s.ffmpegPid = &pid

	go m.monitor(s)
	go m.wait(s, cmd)

	return m.snapshotLocked(s), nil
}

// keyframeLead — на сколько раньше найденного ключевого кадра ставится -ss.
//
// Отступ обязателен, и это измерено 18.08.2026, а не подстраховка: -ss ровно
// на pts ключевого кадра САМ КАДР НЕ ВКЛЮЧАЕТ. На s03e23 «Друзей» с ключевыми
// кадрами 1190.773 и 1198.572 запрос -ss 1190.773 дал первый видеокадр
// на 7.8 с позже — ffmpeg отбросил кадр на границе и уехал к следующему,
// то есть выравнивание без отступа делало ровно ту дырку, ради которой
// затевалось. С -ss 1190.673 первый кадр приходит сразу.
//
// Величина некритична сверху: промахнувшись назад через предыдущий ключевой
// кадр, мы начнём с него — раньше на десятую секунды, без всякой дырки.
// Критична снизу, поэтому не микроскопическая: pts в контейнере и pts
// в выводе ffprobe округляются по-разному.
const keyframeLead = 0.1

// alignSeek подтягивает запрошенную секунду на ключевой кадр — единственное,
// что отделяет перемотку копированием от перемотки через libx264.
//
// ffprobe тут не запускается впустую: если формат исходника всё равно не
// копируется (не h264, 10 бит, чересстрочка) или копирование выключено
// рычагом, точка остаётся как просили, и сеанс идёт перекодированием,
// где выравнивание не нужно — libx264 попадает в запрошенную секунду точно.
func (m *Manager) alignSeek(opts StartOptions, v *media.VideoInfo) media.SeekPoint {
	seek := media.SeekPoint{Start: opts.Start}
	if seek.AtStart() || m.Keyframe == nil || !m.AllowCopy || !media.VideoFormatCopyable(v) {
		return seek
	}
	kf, ok := m.Keyframe(opts.Index, v.Index, seek.Start)
	if !ok {
		return seek
	}
	start := kf - keyframeLead
	if start < 0 {
		start = 0
	}
	return media.SeekPoint{Start: start, OnKeyframe: true}
}

// monitor раз в 500 мс пересчитывает готовые сегменты.
func (m *Manager) monitor(s *Session) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopMonitor:
			return
		case <-ticker.C:
			m.mu.Lock()
			pollSegments(s, m.now())
			s.advancePhase()
			m.mu.Unlock()
		}
	}
}

// wait разбирает завершение ffmpeg.
//
// Порядок здесь несущий и повторяет Node: сначала монитор ГАРАНТИРОВАННО
// остановлен, и только потом финальный пересчёт. Без рандеву через monitorDone
// последний тик монитора мог бы уехать после записи итогов.
func (m *Manager) wait(s *Session, cmd *exec.Cmd) {
	err := cmd.Wait()

	s.stopOnce.Do(func() { close(s.stopMonitor) })
	<-s.monitorDone

	m.mu.Lock()
	defer m.mu.Unlock()

	s.exitCode = exitCodeNodeStyle(cmd.ProcessState)
	s.exited = true
	if s.killTimer != nil {
		s.killTimer.Stop()
		s.killTimer = nil
	}

	wasStopped := s.stopReason != ""
	if !wasStopped && isFatalExit(s.exitCode) {
		text := s.stderr.String()
		if text == "" {
			text = fmt.Sprintf("ffmpeg exited %d", derefInt(s.exitCode))
		}
		text = tailRunes(text, errorTailLimit)
		s.errMsg = &text
	} else if !wasStopped && s.errMsg == nil {
		s.phase = phaseFinished
	}
	_ = err

	pollSegments(s, m.now())
	s.advancePhase()
	now := m.now()
	s.lastOutputAt = &now

	if m.active == s {
		// Каталог сеанса, который ещё смотрят, не трогаем: ffmpeg часто
		// обгоняет реальное время, и плеер будет добирать уже готовые
		// сегменты ещё долго после выхода процесса.
		log.Printf("HLS [%d] ffmpeg finished, keeping session %s", s.index, s.id)
		return
	}
	m.scheduleCleanupLocked(s, cleanupDelay)
}

// Stop останавливает активный сеанс. Возвращает его id, если он был.
func (m *Manager) Stop() *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	s := m.active
	m.stopLocked(s, reasonStopped)
	m.scheduleCleanupLocked(s, cleanupDelay)
	m.active = nil
	id := s.id
	return &id
}

// stopLocked помечает причину, гасит монитор и просит ffmpeg завершиться.
func (m *Manager) stopLocked(s *Session, reason string) {
	if s.stopReason == "" {
		s.stopReason = reason
	}
	s.stopOnce.Do(func() { close(s.stopMonitor) })

	if s.cmd == nil || s.cmd.Process == nil || s.exited {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)

	// ffmpeg, заблокированный на чтении из подвисшего /raw, на SIGTERM может
	// не отреагировать вовсе — и продолжит перекодировать и тянуть торрент
	// уже после «остановки».
	if s.killTimer == nil {
		s.killTimer = time.AfterFunc(killGrace, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			s.killTimer = nil
			if s.exited {
				return
			}
			log.Printf("ffmpeg [%d] ignored SIGTERM, sending SIGKILL", s.index)
			if s.cmd != nil && s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		})
	}
}

// scheduleCleanupLocked ставит удаление каталога сеанса.
func (m *Manager) scheduleCleanupLocked(s *Session, delay time.Duration) {
	if s.cleanupTimer != nil {
		return
	}
	s.cleanupTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// Обнуление ДО возможного перевзвода обязательно: иначе проверка
		// выше съест перевзвод и каталог не удалится уже никогда.
		s.cleanupTimer = nil
		if m.closed {
			return
		}
		// Живой ffmpeg тоже держит каталог: удалить его под процессом — получить
		// поток ошибок записи вместо чистого выхода.
		if m.active == s || !s.exited {
			m.scheduleCleanupLocked(s, cleanupRetry)
			return
		}
		delete(m.sessions, s.id)
		os.RemoveAll(s.dir)
	})
}

// Get отдаёт снимок сеанса по id.
func (m *Manager) Get(id string) (Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Snapshot{}, false
	}
	return m.snapshotLocked(s), true
}

// ActiveSnapshot отдаёт снимок активного сеанса или nil.
//
// Считается на месте, а не хранится зеркалом: зеркало отставало и продолжало
// показывать уже мёртвый сеанс.
func (m *Manager) ActiveSnapshot() *Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	snap := m.snapshotLocked(m.active)
	return &snap
}

// Progress отдаёт ход работы по сеансу. Пустой id — активный сеанс.
func (m *Manager) Progress(id string) (Progress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.active
	if id != "" {
		var ok bool
		if s, ok = m.sessions[id]; !ok {
			return Progress{}, false
		}
	}
	if s == nil {
		return Progress{}, false
	}
	return m.progressLocked(s), true
}

func (m *Manager) progressLocked(s *Session) Progress {
	p := Progress{
		ID:              s.id,
		Name:            s.name,
		Index:           s.index,
		State:           s.state(),
		Start:           s.start,
		Segments:        s.segments,
		StartupSegments: StartupSegments,
		StartupTargetMs: int64(StartupSegments) * SegmentSeconds * 1000,
		Pipeline:        s.pipeline,
	}
	if d := m.downloaded() - s.downloadedAtStart; d > 0 {
		p.DownloadedSinceStart = d
	}

	block, seen := s.progress.snapshot()
	if !seen {
		// ffmpeg не отчитался ещё ни разу: либо не стартовал, либо сидит
		// на чтении входа и ждёт данных из роя. Ноль сюда класть нельзя —
		// это разные вещи, и на экране они выглядят по-разному.
		return p
	}
	if block.HasTime {
		encoded := block.OutTimeMs
		p.EncodedMs = &encoded
	}
	if block.HasSpeed {
		speed := block.Speed
		p.Speed = &speed
	}
	// Остаток считается только до первой картинки и только когда есть чем:
	// после старта воспроизведения ffmpeg работает вперёд без всякого срока,
	// и «осталось N секунд» там означало бы неправду.
	if p.EncodedMs != nil && p.Speed != nil && *p.Speed > 0 && s.segments < StartupSegments {
		if left := p.StartupTargetMs - *p.EncodedMs; left > 0 {
			eta := int64(float64(left) / *p.Speed)
			p.EtaMs = &eta
		}
	}
	return p
}

// SessionDir отдаёт каталог сеанса — по нему отдаются файлы /hls/:id/*.
func (m *Manager) SessionDir(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return "", false
	}
	return s.dir, true
}

func (m *Manager) snapshotLocked(s *Session) Snapshot {
	snap := Snapshot{
		ID:        s.id,
		Index:     s.index,
		Name:      s.name,
		Audio:     s.audio,
		Sub:       s.sub,
		VideoMode: s.videoMode,
		AudioMode: s.audioMode,
		Start:     s.start,
		State:     s.state(),
		StartedAt: s.startedAt,
		Segments:  s.segments,
		BytesOut:  s.bytesOut,

		LastOutputAt: s.lastOutputAt,
		FFmpegPID:    s.ffmpegPid,
		ExitCode:     s.exitCode,
		Error:        s.errMsg,

		Playlist: "/hls/" + s.id + "/" + PlaylistName,
		Format:   "HLS/MPEG-TS",
	}
	if s.sub != "off" {
		sp := "/hls/" + s.id + "/index_vtt.m3u8"
		snap.SubtitlePlaylist = &sp
		snap.Format = "HLS/MPEG-TS + app-rendered WebVTT"
	}
	if d := m.downloaded() - s.downloadedAtStart; d > 0 {
		snap.DownloadedSinceStart = d
	}
	return snap
}

// Shutdown убивает всё и стирает каталоги.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for id, s := range m.sessions {
		if s.cleanupTimer != nil {
			s.cleanupTimer.Stop()
			s.cleanupTimer = nil
		}
		if s.killTimer != nil {
			s.killTimer.Stop()
			s.killTimer = nil
		}
		s.stopOnce.Do(func() { close(s.stopMonitor) })
		if s.cmd != nil && s.cmd.Process != nil && !s.exited {
			_ = s.cmd.Process.Kill()
		}
		// Каталог НЕ удаляется: его подберёт следующий процесс и продолжит
		// отдавать сегменты, чтобы выкатка не обрывала просмотр.
		// Мусор за собой уберёт он же — по TTL после подбора.
		delete(m.sessions, id)
	}
	m.active = nil
}

func (m *Manager) now() int64 {
	if m.NowMilli != nil {
		return m.NowMilli()
	}
	return time.Now().UnixMilli()
}

func (m *Manager) downloaded() int64 {
	if m.Downloaded != nil {
		return m.Downloaded()
	}
	return 0
}

func (m *Manager) tmpDir() string {
	if m.TmpDir != "" {
		return m.TmpDir
	}
	return os.TempDir()
}

func (m *Manager) ffmpeg() string {
	if m.FFmpeg != "" {
		return m.FFmpeg
	}
	return "ffmpeg"
}

// newID повторяет `${Date.now().toString(36)}-${random}`: 6 символов из [0-9a-z].
// Регистр важен — id должен пройти и маршрут /hls/:id, и проверку имени каталога.
func (m *Manager) newID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = alphabet[0]
		}
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return jscompat.Base36(m.now()) + "-" + string(buf)
}

// exitCodeNodeStyle переводит завершение процесса в то, что видел Node.
//
// child.on('close', code) даёт null, когда процесс убит сигналом, а
// ProcessState.ExitCode() даёт -1. Без этого перевода, во-первых, в снимке
// оказалось бы "exitCode":-1 вместо null, а во-вторых — убитый по SIGKILL
// сеанс ложно попал бы в состояние error, потому что -1 не равен ни 255, ни 143.
func exitCodeNodeStyle(ps *os.ProcessState) *int {
	if ps == nil {
		return nil
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return nil
	}
	code := ps.ExitCode()
	return &code
}

// isFatalExit — `code && code !== 255 && code !== 143`.
// 255 отдаёт сам ffmpeg по SIGTERM, 143 приходит из-под шелла: оба это
// нормальная остановка, а не сбой.
func isFatalExit(code *int) bool {
	if code == nil {
		return false
	}
	return *code != 0 && *code != 255 && *code != 143
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func codeOr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func audioCode(a *media.AudioTrack) string {
	if a == nil {
		return ""
	}
	return a.Code
}

func subCode(s *media.SubtitleTrack) string {
	if s == nil {
		return ""
	}
	return s.Code
}
