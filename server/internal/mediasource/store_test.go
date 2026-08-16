package mediasource

import (
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
			a := &Anacrolix{dataDir: store, persistStore: c.persist, stop: make(chan struct{})}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}

			_, err := os.Stat(payload)
			switch {
			case c.survives && err != nil:
				t.Errorf("данные должны были уцелеть, но их нет: %v", err)
			case !c.survives && err == nil:
				t.Error("данные должны были стереться, но остались")
			}
		})
	}
}

// TestCloseIsIdempotent — Close зовётся и из shutdown, и из defer в main.
func TestCloseIsIdempotent(t *testing.T) {
	a := &Anacrolix{dataDir: t.TempDir(), persistStore: true, stop: make(chan struct{})}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("повторный Close упал: %v", err)
	}
}
