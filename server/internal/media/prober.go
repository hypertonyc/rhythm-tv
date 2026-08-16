package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"time"
)

// ErrNoVideoStream — та самая строка, которую /api/start отдаёт в поле error,
// а браузерный клиент показывает пользователю дословно.
var ErrNoVideoStream = errors.New("No video stream")

// Request — вход одного разбора. Имя и соседние серии знает слой торрента,
// поэтому они приходят снаружи: сам Prober про торренты ничего не знает.
type Request struct {
	Index int
	Name  string
	Next  *int
	Prev  *int
}

// Prober запускает ffprobe и кэширует разбор.
//
// Один ffprobe на файл: телевизор и веб-клиент легко просят один индекс
// одновременно (плюс пинг /api/probe/next перед концом эпизода), а каждый
// лишний ffprobe — это отдельное чтение торрента.
type Prober struct {
	// RawURL отдаёт http://127.0.0.1:<PORT>/raw/<index>. ffprobe читает торрент
	// петлёй через наш же HTTP-сервер.
	RawURL func(index int) string
	// Timeout выбран чуть меньше 30-секундного XHR-таймаута телевизора,
	// чтобы клиент увидел внятную ошибку, а не оборванный запрос.
	Timeout time.Duration
	// Binary — имя ffprobe; подменяется в тестах.
	Binary string

	mu      sync.Mutex
	entries map[int]*probeEntry
}

type probeEntry struct {
	ready  chan struct{}
	result *Result
	err    error
}

// Probe разбирает файл, склеивая одновременные запросы в один запуск ffprobe.
//
// Кэшируются ТОЛЬКО успехи. Это не упущение, а поведение Node: там результат
// клали в кэш после await, а из карты «в процессе» удаляли в finally, — так что
// после ошибки следующий запрос пробует заново. На торренте без пиров это
// разница между «подождать и получить фильм» и «залипнуть навсегда».
func (p *Prober) Probe(ctx context.Context, req Request) (*Result, error) {
	p.mu.Lock()
	if p.entries == nil {
		p.entries = make(map[int]*probeEntry)
	}
	if e, ok := p.entries[req.Index]; ok {
		p.mu.Unlock()
		select {
		case <-e.ready:
			return e.result, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e := &probeEntry{ready: make(chan struct{})}
	p.entries[req.Index] = e
	p.mu.Unlock()

	e.result, e.err = p.run(ctx, req)
	close(e.ready)

	if e.err != nil {
		p.mu.Lock()
		delete(p.entries, req.Index)
		p.mu.Unlock()
	}
	return e.result, e.err
}

func (p *Prober) run(ctx context.Context, req Request) (*Result, error) {
	binary := p.Binary
	if binary == "" {
		binary = "ffprobe"
	}
	out, err := runCapture(ctx, binary, []string{
		"-v", "error",
		"-show_streams",
		"-show_format",
		"-of", "json",
		p.RawURL(req.Index),
	}, p.Timeout)
	if err != nil {
		return nil, err
	}
	return ParseProbe(out, req.Index, req.Name, req.Next, req.Prev)
}

// runCapture — порт runCapture() из server.mjs.
//
// Тексты ошибок воспроизведены дословно: /api/probe отдаёт их в поле error,
// а браузерный клиент печатает это поле как есть.
func runCapture(ctx context.Context, command string, args []string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	// Без таймера запрос может не завершиться никогда: ffprobe читает файл
	// через /raw, то есть через торрент, и на раздаче без пиров просто висит.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// CommandContext по умолчанию бьёт SIGKILL — как и Node в этом месте.

	err := cmd.Run()

	// Проверка таймаута идёт ПЕРЕД кодом возврата: убитый по таймауту процесс
	// вернёт ненулевой код, но пользователю надо сказать про таймаут.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, fmt.Errorf("%s timed out after %ds", command, int(math.Round(timeout.Seconds())))
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s exited with %d: %s", command, exitErr.ExitCode(), stderr.String())
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
