package reclaim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJournalSurvivesRestart — без этого каждая выкатка обнуляла бы порядок
// выселения, и первая же чистка после неё удаляла бы что попало.
func TestJournalSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tms-watched")
	at := time.Date(2026, 8, 17, 20, 30, 0, 0, time.UTC)

	j := LoadJournal(path)
	j.Touch("Друзья/Сезон 01/s01e01.mkv", at)
	j.Touch("Друзья/Сезон 01/s01e02.mkv", at.Add(time.Hour))

	again := LoadJournal(path)
	if again.Len() != 2 {
		t.Fatalf("после перезапуска записей %d, ожидалось 2", again.Len())
	}
	got, ok := again.At("Друзья/Сезон 01/s01e01.mkv")
	if !ok || got != at.UnixMilli() {
		t.Errorf("отметка %d (найдена=%v), ожидалась %d", got, ok, at.UnixMilli())
	}
	if _, ok := again.At("Друзья/Сезон 01/s01e03.mkv"); ok {
		t.Error("нашлась отметка о серии, которую не смотрели")
	}
}

// TestJournalSurvivesGarbage — битый журнал не должен ронять сервер: без
// истории просмотров чистка переходит на mtime и продолжает работать.
func TestJournalSurvivesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tms-watched")
	if err := os.WriteFile(path, []byte("это не json"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := LoadJournal(path)
	if j.Len() != 0 {
		t.Fatalf("из мусора вычиталось %d записей", j.Len())
	}
	// И пишется поверх мусора как ни в чём не бывало.
	j.Touch("Друзья/s01e01.mkv", time.Now())
	if LoadJournal(path).Len() != 1 {
		t.Error("после битого журнала запись не сохранилась")
	}
}

// TestJournalSurvivesReadOnlyDir — каталог библиотеки может быть смонтирован
// только на чтение; терять из-за этого запуск серии нельзя.
func TestJournalSurvivesReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	j := LoadJournal(filepath.Join(dir, ".tms-watched"))
	j.Touch("Друзья/s01e01.mkv", time.Now())
	if _, ok := j.At("Друзья/s01e01.mkv"); !ok {
		t.Error("отметка не попала даже в память")
	}
}
