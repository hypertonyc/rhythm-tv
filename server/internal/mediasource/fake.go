package mediasource

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// Fake — источник поверх обычных файлов на диске.
//
// Нужен затем, что весь HTTP-слой иначе нельзя проверить без роя, трекеров
// и ожидания пиров: тесты /raw, /api/files и /api/probe гоняются на нём,
// а ffmpeg и ffprobe читают его через тот же loopback, что и настоящий торрент.
type Fake struct {
	name  string
	paths []string
	files []File

	mu    sync.Mutex
	stats Stats
	// OpenCount считает выданные Reader'ы, ReadersOpen — незакрытые.
	// Второй счётчик и есть смысл этого типа в тестах: утёкший Reader
	// у anacrolix навсегда оставляет окно приоритета, и торрент качается
	// после ухода клиента. Тест на утечку ловит это здесь.
	OpenCount   int
	ReadersOpen int

	// NotReady эмулирует торрент, у которого ещё нет метаданных.
	NotReady bool
}

// NewFake собирает источник из перечисленных файлов. Имя торрента — name,
// имена файлов берутся из последнего сегмента пути, как и у настоящего.
func NewFake(name string, paths ...string) (*Fake, error) {
	f := &Fake{name: name, paths: paths}
	for i, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		f.files = append(f.files, File{Index: i, Name: filepath.Base(p), Length: st.Size()})
	}
	return f, nil
}

func (f *Fake) Ready() bool  { return !f.NotReady }
func (f *Fake) Name() string { return f.name }

func (f *Fake) Files() []File {
	if f.NotReady {
		return nil
	}
	out := make([]File, len(f.files))
	copy(out, f.files)
	return out
}

func (f *Fake) Open(index int) (Reader, error) {
	if f.NotReady {
		return nil, ErrNotReady
	}
	if index < 0 || index >= len(f.paths) {
		return nil, os.ErrNotExist
	}
	fh, err := os.Open(f.paths[index])
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.OpenCount++
	f.ReadersOpen++
	f.mu.Unlock()
	return &fakeReader{f: fh, owner: f}, nil
}

func (f *Fake) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

// SetStats подставляет показания для тестов /api/status.
func (f *Fake) SetStats(s Stats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats = s
}

// Leaked сообщает, остались ли незакрытые Reader'ы.
func (f *Fake) Leaked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ReadersOpen
}

func (f *Fake) Close() error { return nil }

type fakeReader struct {
	f     *os.File
	owner *Fake
	once  sync.Once
}

func (r *fakeReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.f.Read(p)
}

func (r *fakeReader) Seek(off int64, whence int) (int64, error) { return r.f.Seek(off, whence) }
func (r *fakeReader) SetReadahead(int64)                        {}

func (r *fakeReader) Close() error {
	var err error
	r.once.Do(func() {
		err = r.f.Close()
		r.owner.mu.Lock()
		r.owner.ReadersOpen--
		r.owner.mu.Unlock()
	})
	return err
}

var (
	_ Source = (*Fake)(nil)
	_ Source = (*Anacrolix)(nil)
	_ Reader = (*fakeReader)(nil)
)
