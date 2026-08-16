package mediasource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCloseRespectsPersistStore — проверка того, перепутать в чём означало бы
// стереть уже скачанное. На проде в хранилище десятки гигабайт, и ошибка
// в знаке этого условия обнаружилась бы только повторной закачкой сериала.
func TestCloseRespectsPersistStore(t *testing.T) {
	for _, c := range []struct {
		name     string
		persist  bool
		survives bool
	}{
		{name: "по умолчанию стирает, как Node", persist: false, survives: false},
		{name: "TORRENT_STORE_PERSIST=1 сохраняет", persist: true, survives: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			store := filepath.Join(dir, "store")
			if err := os.MkdirAll(store, 0o777); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(store, "piece.dat")
			if err := os.WriteFile(payload, []byte("уже скачанное"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Клиент не поднимаем: проверяется ровно ветка очистки в Close,
			// а сеть и рой к ней отношения не имеют.
			c := &Client{dataDir: store, persistStore: c.persist}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}

			_, err := os.Stat(payload)
			switch {
			case c.persistStore && err != nil:
				t.Errorf("данные должны были уцелеть, но их нет: %v", err)
			case !c.persistStore && err == nil:
				t.Error("данные должны были стереться, но остались")
			}
		})
	}
}

// TestCloseIsIdempotent — Close зовётся и из shutdown, и из defer в main.
func TestCloseIsIdempotent(t *testing.T) {
	c := &Client{dataDir: t.TempDir(), persistStore: true}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("повторный Close упал: %v", err)
	}
}

// TestTorrentCloseDoesNotTouchStore фиксирует, что смена активного торрента
// не трогает диск. Хранилище общее на всю библиотеку: очистка при переключении
// выбросила бы гигабайты остальных торрентов.
func TestTorrentCloseDoesNotTouchStore(t *testing.T) {
	store := t.TempDir()
	payload := filepath.Join(store, "piece.dat")
	if err := os.WriteFile(payload, []byte("чужие гигабайты"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tor := &Torrent{stop: make(chan struct{}), ctx: ctx, cancel: cancel}
	if err := tor.Close(); err != nil {
		t.Fatal(err)
	}
	// Повторный Close случается на выключении процесса: библиотека закрывает
	// активный торрент, а следом за ней тот же торрент закрывает клиент.
	if err := tor.Close(); err != nil {
		t.Fatalf("повторный Close упал: %v", err)
	}

	if _, err := os.Stat(payload); err != nil {
		t.Errorf("данные соседнего торрента исчезли: %v", err)
	}
	if ctx.Err() == nil {
		t.Error("контекст торрента должен быть отменён: иначе читатели не проснутся")
	}
}
