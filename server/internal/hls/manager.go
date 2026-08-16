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
		if err := subs.WriteSession(dir, subtitle.SourcePath, opts.Start, meta.Duration); err != nil {
			log.Printf("HLS [%d] субтитры %s: %v", opts.Index, subtitle.SourcePath, err)
			subtitle = nil
		}
	}

	copyVideo := media.CanCopyVideo(meta.Video, opts.Start, m.AllowCopy)
	copyAudio := audio != nil && media.CanCopyAudio(audio, opts.Start, m.AllowCopy)

	args := BuildArgs(Params{
		RawURL:     m.RawURL(opts.Index),
		Dir:        dir,
		VideoIndex: meta.Video.Index,
		Audio:      audio,
		Subtitle:   subtitle,
		Start:      opts.Start,
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
		jscompat.ToFixed(opts.Start, 1), videoMode, audioMode,
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
		start:             opts.Start,
		downloadedAtStart: m.downloaded(),
		startedAt:         m.now(),
		phase:             phasePreparing,
		stderr:            &ringWriter{limit: stderrLimit},
		stopMonitor:       make(chan struct{}),
		monitorDone:       make(chan struct{}),
	}
	m.sessions[id] = s
	m.active = s
	// Пишется до запуска ffmpeg: каталог должен быть опознаваем, даже если
	// процесс не стартует вовсе.
	writeManifest(s)

	cmd := exec.Command(m.ffmpeg(), args...)
	// stdout выбрасывается, stderr копится в кольцевом буфере.
	// StderrPipe использовать нельзя: он запрещает чтение после Wait.
	cmd.Stderr = s.stderr
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

		Playlist: "/hls/" + s.id + "/index.m3u8",
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
