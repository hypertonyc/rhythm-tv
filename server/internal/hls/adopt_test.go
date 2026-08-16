package hls

import (
	"os"
	"path/filepath"
	"testing"
)

// newSessionDir лепит каталог сеанса так, как его оставил бы прежний процесс.
func newSessionDir(t *testing.T, tmp, id string, segments int, withManifest bool) string {
	t.Helper()
	dir := filepath.Join(tmp, "tms-hls-"+id)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		name := filepath.Join(dir, "seg"+pad5(i)+".ts")
		if err := os.WriteFile(name, make([]byte, 1000+i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withManifest {
		s := &Session{
			id: id, dir: dir, name: "S07E12 - Проверка.mp4", index: 146,
			audio: "rus", sub: "rus", videoMode: "copy", audioMode: "copy",
			start: 0, startedAt: 1786870000000,
		}
		writeManifest(s)
	}
	return dir
}

func pad5(n int) string {
	s := "00000" + itoaSmall(n)
	return s[len(s)-5:]
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAdoptOrphans — ради этого всё и делалось: после выкатки сегменты
// прежнего сеанса должны продолжать отдаваться, а не превращаться в 404.
func TestAdoptOrphans(t *testing.T) {
	tmp := t.TempDir()
	dir := newSessionDir(t, tmp, "msvnwngu-fq8cm6", 3, true)

	m := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 1786870500000 }}
	if n := m.AdoptOrphans(); n != 1 {
		t.Fatalf("подобрано %d сеансов, ожидался 1", n)
	}

	got, ok := m.SessionDir("msvnwngu-fq8cm6")
	if !ok || got != dir {
		t.Fatalf("SessionDir = %q, %v; ожидался %q", got, ok, dir)
	}

	snap, ok := m.Get("msvnwngu-fq8cm6")
	if !ok {
		t.Fatal("Get не нашёл подобранный сеанс")
	}
	// finished — «больше сегментов не будет, играй что есть». Именно на этом
	// состоянии клиент перестаёт ждать и открывает плейлист.
	if snap.State != "finished" {
		t.Errorf("state = %q, ожидалось finished", snap.State)
	}
	if snap.Segments != 3 {
		t.Errorf("segments = %d, ожидалось 3 (посчитаны по файлам)", snap.Segments)
	}
	if snap.Name != "S07E12 - Проверка.mp4" || snap.Index != 146 {
		t.Errorf("метаданные не восстановились: %q / %d", snap.Name, snap.Index)
	}
	if snap.Playlist != "/hls/msvnwngu-fq8cm6/index.m3u8" {
		t.Errorf("playlist = %q", snap.Playlist)
	}
}

// TestAdoptedSessionIsNotActive — самая коварная ловушка этой затеи.
// Активность видна снаружи как playback != null, и по ней пайплайн решает,
// можно ли выкатываться. Если подобранный сеанс станет активным, сервер
// после первой же выкатки навсегда скажет «занят» и заблокирует все следующие.
func TestAdoptedSessionIsNotActive(t *testing.T) {
	tmp := t.TempDir()
	newSessionDir(t, tmp, "aaaa-bbbb", 2, true)

	m := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 1 }}
	m.AdoptOrphans()

	if snap := m.ActiveSnapshot(); snap != nil {
		t.Errorf("подобранный сеанс стал активным: %+v — деплой заблокируется навсегда", snap)
	}
}

// TestAdoptSkipsDirsWithoutManifest — каталог без манифеста восстановить нечем,
// он остался от аварийного выхода и только занимает диск.
func TestAdoptSkipsDirsWithoutManifest(t *testing.T) {
	tmp := t.TempDir()
	orphan := newSessionDir(t, tmp, "no-manifest", 1, false)
	newSessionDir(t, tmp, "with-manifest", 1, true)

	m := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 1 }}
	if n := m.AdoptOrphans(); n != 1 {
		t.Fatalf("подобрано %d, ожидался 1", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("каталог без манифеста должен был удалиться")
	}
}

// TestAdoptIgnoresForeignDirs — в TMPDIR лежит не только наше.
func TestAdoptIgnoresForeignDirs(t *testing.T) {
	tmp := t.TempDir()
	foreign := filepath.Join(tmp, "something-else")
	if err := os.MkdirAll(foreign, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "tms-hls-not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 1 }}
	if n := m.AdoptOrphans(); n != 0 {
		t.Errorf("подобрано %d, ожидался 0", n)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("чужой каталог не должен быть тронут")
	}
}

// TestShutdownKeepsDirsForSuccessor — Shutdown раньше удалял каталоги,
// и именно поэтому выкатка обрывала просмотр.
func TestShutdownKeepsDirsForSuccessor(t *testing.T) {
	tmp := t.TempDir()
	dir := newSessionDir(t, tmp, "cccc-dddd", 2, true)

	m := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 1 }}
	m.AdoptOrphans()
	m.Shutdown()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("каталог удалён при Shutdown — преемнику нечего будет подобрать: %v", err)
	}

	// И преемник действительно его подберёт.
	m2 := &Manager{TmpDir: tmp, NowMilli: func() int64 { return 2 }}
	if n := m2.AdoptOrphans(); n != 1 {
		t.Errorf("преемник подобрал %d сеансов, ожидался 1", n)
	}
}
