package hls

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Каталоги сеансов переживают перезапуск сервера намеренно.
//
// Раньше Shutdown() их удалял, и выкатка обрывала просмотр: у нового процесса
// нет состояния сеансов, поэтому на /hls/<id>/... он отвечал 404, а файлов
// к тому моменту уже не было. Теперь каталоги остаются, новый процесс
// их подбирает и продолжает отдавать. Для серии, которую ffmpeg успел
// дотранскодировать (в режиме copy это первые полминуты), выкатка становится
// незаметной вовсе.
//
// Подобранный сеанс НЕ становится активным. Это важно: активность видна
// снаружи как playback != null, а по этому полю пайплайн решает, можно ли
// выкатываться. Иначе после первой же выкатки сервер навсегда сообщал бы
// «занят» и заблокировал все следующие.
const manifestName = "session.json"

// manifest — то, что нельзя восстановить из одних имён файлов.
// Формат внутренний, контракта с телевизором не касается.
type manifest struct {
	ID                string  `json:"id"`
	Index             int     `json:"index"`
	Name              string  `json:"name"`
	Audio             string  `json:"audio"`
	Sub               string  `json:"sub"`
	VideoMode         string  `json:"videoMode"`
	AudioMode         string  `json:"audioMode"`
	Start             float64 `json:"start"`
	StartedAt         int64   `json:"startedAt"`
	DownloadedAtStart int64   `json:"downloadedAtStart"`
}

// writeManifest кладёт описание сеанса рядом с сегментами.
// Ошибка не фатальна: без манифеста сеанс просто не переживёт перезапуск.
func writeManifest(s *Session) {
	m := manifest{
		ID: s.id, Index: s.index, Name: s.name,
		Audio: s.audio, Sub: s.sub,
		VideoMode: s.videoMode, AudioMode: s.audioMode,
		Start: s.start, StartedAt: s.startedAt,
		DownloadedAtStart: s.downloadedAtStart,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, manifestName), b, 0o644)
}

// AdoptOrphans подбирает каталоги сеансов, оставшиеся от прежнего процесса.
//
// Возвращает число подобранных. Вызывать один раз при старте, до того как
// сервер начнёт принимать запросы.
func (m *Manager) AdoptOrphans() int {
	entries, err := os.ReadDir(m.tmpDir())
	if err != nil {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]*Session)
	}

	adopted := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "tms-hls-") {
			continue
		}
		dir := filepath.Join(m.tmpDir(), e.Name())
		id := strings.TrimPrefix(e.Name(), "tms-hls-")
		if id == "" || m.sessions[id] != nil {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(dir, manifestName))
		if err != nil {
			// Без манифеста восстановить снимок нечем. Каталог, скорее всего,
			// остался от аварийного выхода — просто убираем.
			os.RemoveAll(dir)
			continue
		}
		var mf manifest
		if err := json.Unmarshal(raw, &mf); err != nil || mf.ID != id {
			os.RemoveAll(dir)
			continue
		}

		s := &Session{
			id: id, dir: dir, name: mf.Name, index: mf.Index,
			audio: mf.Audio, sub: mf.Sub,
			videoMode: mf.VideoMode, audioMode: mf.AudioMode,
			start: mf.Start, startedAt: mf.StartedAt,
			downloadedAtStart: mf.DownloadedAtStart,
			// Процесса нет: он умер вместе с прежним контейнером. Отсюда
			// exited=true и фаза finished — «больше сегментов не будет,
			// играй то, что есть». Клиент на finished перестаёт ждать
			// и открывает плейлист, что нам и нужно.
			phase:  phaseFinished,
			exited: true,
			// Монитор не запускается, но каналы должны быть закрыты:
			// на них смотрит Shutdown.
			stopMonitor: make(chan struct{}),
			monitorDone: make(chan struct{}),
			stderr:      &ringWriter{limit: stderrLimit},
		}
		close(s.stopMonitor)
		close(s.monitorDone)
		s.stopOnce.Do(func() {})

		// Счётчики восстанавливаем по файлам на диске — это тот же обход,
		// что делает монитор на первом тике.
		pollSegments(s, m.now())

		m.sessions[id] = s
		// Чистка отложена надолго: зритель может досматривать серию,
		// начатую до выкатки. Час с запасом покрывает эпизод.
		m.scheduleCleanupLocked(s, adoptedTTL)
		adopted++
	}
	return adopted
}

// adoptedTTL — сколько живёт подобранный сеанс, прежде чем его каталог удалят.
// Серия идёт около двадцати минут; час берётся с запасом на паузы.
const adoptedTTL = time.Hour
