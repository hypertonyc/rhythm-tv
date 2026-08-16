package subs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pack раскладывает файлы субтитров по каталогу и отдаёт библиотеку над ним.
func pack(t *testing.T, files map[string]string) (*Library, string) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(root), root
}

// TestTracksForMatchesByName — связь серии и субтитров только по имени.
func TestTracksForMatchesByName(t *testing.T) {
	lib, root := pack(t, map[string]string{
		"frnds_sub_rus_s01-s10/S01/s01e01 - The Pilot.srt":                            sampleSRT,
		"frnds_sub_rus_s01-s10/S01/s01e02 - The One with the Sonogram at the End.srt": sampleSRT,
		"frnds_sub_rus_s01-s10/.DS_Store":                                             "мусор",
	})

	tracks := lib.TracksFor("s01e01 - The Pilot.mkv")
	if len(tracks) != 1 {
		t.Fatalf("дорожек %d, ожидалась одна: %+v", len(tracks), tracks)
	}
	got := tracks[0]
	if got.Code != "rus" || got.Label != "Russian" || got.Codec != "srt" {
		t.Errorf("дорожка: %+v", got)
	}
	if want := filepath.Join(root, "frnds_sub_rus_s01-s10", "S01", "s01e01 - The Pilot.srt"); got.SourcePath != want {
		t.Errorf("путь %q, ожидался %q", got.SourcePath, want)
	}
	if !got.External() {
		t.Error("дорожка обязана считаться внешней")
	}
	if got.Index != -1 {
		t.Errorf("Index %d: у внешней дорожки нет потока в файле", got.Index)
	}

	if tracks := lib.TracksFor("s09e23-24 - The One in Barbados.mkv"); tracks != nil {
		t.Errorf("для серии без субтитров вернулось %+v", tracks)
	}
}

// TestTracksForIgnoresCase — раздача и пак называют серию по-разному
// в мелочах регистра куда чаще, чем хотелось бы.
func TestTracksForIgnoresCase(t *testing.T) {
	lib, _ := pack(t, map[string]string{"rus/S01E01.SRT": sampleSRT})
	if len(lib.TracksFor("s01e01.mkv")) != 1 {
		t.Error("регистр не должен мешать совпадению")
	}
}

// TestLanguageFromDirectory — язык берётся из названия каталога, и «rus»
// внутри «frnds_sub_rus_s01-s10» обязан находиться: по всему имени целиком
// \brus\b не совпадает, подчёркивание считается частью слова.
func TestLanguageFromDirectory(t *testing.T) {
	for _, c := range []struct {
		path, code, label string
	}{
		{"frnds_sub_rus_s01-s10/S01/episode.srt", "rus", "Russian"},
		{"rus/episode.srt", "rus", "Russian"},
		{"Русские субтитры/episode.srt", "rus", "Russian"},
		{"English/episode.srt", "eng", "English"},
		{"pack/episode.rus.srt", "rus", "Russian"},
		{"pack/episode.eng.srt", "eng", "English"},
		{"pack/episode.srt", "und", "External subtitles"},
	} {
		lib, _ := pack(t, map[string]string{c.path: sampleSRT})
		tracks := lib.TracksFor("episode.mkv")
		if len(tracks) != 1 {
			t.Errorf("%s: дорожек %d", c.path, len(tracks))
			continue
		}
		if tracks[0].Code != c.code || tracks[0].Label != c.label {
			t.Errorf("%s: %s/%s, ожидалось %s/%s",
				c.path, tracks[0].Code, tracks[0].Label, c.code, c.label)
		}
	}
}

// TestLanguageFromLibraryRoot — SUBS_DIR могут нацелить прямо на пак,
// и тогда язык остаётся только в имени самого каталога.
func TestLanguageFromLibraryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frnds_sub_rus_s01-s10")
	if err := os.MkdirAll(filepath.Join(root, "S01"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "S01", "episode.srt"), []byte(sampleSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	tracks := New(root).TracksFor("episode.mkv")
	if len(tracks) != 1 || tracks[0].Code != "rus" {
		t.Errorf("язык из имени каталога библиотеки не подхватился: %+v", tracks)
	}
}

// TestLanguageIgnoresEpisodeTitle — «The One with the English Muffin»
// не английские субтитры, и имя файла в опознании языка не участвует.
func TestLanguageIgnoresEpisodeTitle(t *testing.T) {
	lib, _ := pack(t, map[string]string{
		"pack/s02e16 - The One Where Joey Speaks English.srt": sampleSRT,
	})
	tracks := lib.TracksFor("s02e16 - The One Where Joey Speaks English.mkv")
	if len(tracks) != 1 || tracks[0].Code != "und" {
		t.Errorf("название серии протекло в язык: %+v", tracks)
	}
}

// TestTracksForStableOrder — при двух паках коды дорожек различаются
// суффиксом, а телевизор хранит выбранный код в localStorage. Порядок,
// который зависел бы от обхода каталога, менял бы этот код от старта к старту.
func TestTracksForStableOrder(t *testing.T) {
	lib, _ := pack(t, map[string]string{
		"b-rus/episode.srt": sampleSRT,
		"a-rus/episode.srt": sampleSRT,
	})
	for range 3 {
		tracks := lib.TracksFor("episode.mkv")
		if len(tracks) != 2 {
			t.Fatalf("дорожек %d", len(tracks))
		}
		if filepath.Base(filepath.Dir(tracks[0].SourcePath)) != "a-rus" {
			t.Fatalf("порядок поехал: %+v", tracks)
		}
	}
}

// TestRescan — пак заливают, когда сервер уже работает; перезапускать его
// ради этого не надо.
func TestRescan(t *testing.T) {
	lib, root := pack(t, map[string]string{"rus/episode.srt": sampleSRT})
	now := time.Now()
	lib.now = func() time.Time { return now }

	if len(lib.TracksFor("episode.mkv")) != 1 {
		t.Fatal("первый скан")
	}
	if err := os.WriteFile(filepath.Join(root, "rus", "another.srt"), []byte(sampleSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(lib.TracksFor("another.mkv")) != 0 {
		t.Error("до истечения срока каталог перечитываться не должен")
	}
	now = now.Add(rescanAfter + time.Second)
	if len(lib.TracksFor("another.mkv")) != 1 {
		t.Error("после истечения срока новый файл обязан найтись")
	}
}

// TestMissingDirectory — каталога может не быть вовсе, и это не ошибка.
func TestMissingDirectory(t *testing.T) {
	lib := New(filepath.Join(t.TempDir(), "нет-такого"))
	if got := lib.TracksFor("episode.mkv"); got != nil {
		t.Errorf("вернулось %+v", got)
	}
	if got := lib.Count(); got != 0 {
		t.Errorf("Count() = %d", got)
	}
}

// TestNilLibrary — SUBS_DIR не задан, поведение сервера прежнее.
func TestNilLibrary(t *testing.T) {
	var lib *Library
	if got := lib.TracksFor("episode.mkv"); got != nil {
		t.Errorf("вернулось %+v", got)
	}
	if lib.Count() != 0 || lib.Dir() != "" {
		t.Error("нулевая библиотека обязана молчать")
	}
}
