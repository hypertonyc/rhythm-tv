package mediasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

type deadSwarmReader struct{}

func (deadSwarmReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (deadSwarmReader) Seek(int64, int) (int64, error) { return 0, nil }
func (deadSwarmReader) Close() error                   { return nil }
func (deadSwarmReader) SetReadahead(int64)             {}

// TestWarmupGivesUpOnDeadSwarm — без срока разогрев на раздаче без пиров не кончается,
// и /api/files отвечает 503 до переключения на другой торрент.
func TestWarmupGivesUpOnDeadSwarm(t *testing.T) {
	started := time.Now()
	n, err := warmRead(context.Background(), deadSwarmReader{}, 256<<10, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, ожидался выход по сроку", err)
	}
	if n != 0 {
		t.Errorf("прочитано %d байт из мёртвого роя", n)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("разогрев сдался через %s при сроке 50 мс", elapsed)
	}
}

// TestWarmupStopsWithTorrent — переключение на другой торрент обрывает разогрев и без срока.
func TestWarmupStopsWithTorrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := warmRead(ctx, deadSwarmReader{}, 256<<10, 0)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, ожидалась отмена", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("разогрев пережил снятие торрента")
	}
}
