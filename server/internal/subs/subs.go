// Package subs подсовывает серии субтитры, которых нет внутри её файла.
//
// Зачем: раздача сериала и перевод к нему почти никогда не приезжают вместе.
// В mkv лежат английские дорожки, а русские субтитры существуют отдельным
// паком из .srt-файлов, и единственное, что их связывает с сериями, — имя.
// Поэтому библиотека здесь устроена как каталог, а не как база: положили пак
// в SUBS_DIR — он подхватился, дорожка появилась в меню телевизора рядом
// со встроенными и включается ровно так же.
//
// Наружу пакет отдаёт готовые media.SubtitleTrack с заполненным SourcePath —
// по этому полю остальной сервер и отличает внешнюю дорожку от встроенной.
// Внутри файла таких потоков нет, поэтому ffmpeg о них не знает вовсе:
// WebVTT для сеанса собирается здесь же (см. WriteSession).
package subs

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avdav/torrent-media/server/internal/media"
)

// Расширения, которые считаются субтитрами. Оба разбираются одним парсером:
// формат блоков у SRT и WebVTT общий.
var extensions = map[string]bool{".srt": true, ".vtt": true}

// rescanAfter — как долго живёт разобранный каталог.
//
// Пересканирование нужно ради одного сценария: пак заливают на сервер, пока
// сервер работает. Ждать перезапуска ради этого не надо, а обходить пару сотен
// файлов раз в полминуты дешевле, чем один запрос к торренту.
const rescanAfter = 30 * time.Second

// Library — каталог с файлами субтитров, разложенный по именам серий.
//
// Нулевой указатель — законное состояние: каталога может не быть вовсе,
// и тогда всё поведение сервера остаётся прежним. Поэтому у всех методов
// приёмник проверяется на nil.
type Library struct {
	dir string

	mu        sync.Mutex
	scanned   time.Time
	byEpisode map[string][]media.SubtitleTrack

	// now подменяется в тестах, чтобы не спать полминуты ради пересканирования.
	now func() time.Time
}

// New собирает библиотеку над каталогом. Отсутствие каталога ошибкой не считается.
func New(dir string) *Library {
	if dir == "" {
		return nil
	}
	return &Library{dir: dir, now: time.Now}
}

// Dir отдаёт каталог библиотеки (для логов).
func (l *Library) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// TracksFor отдаёт дорожки, подходящие видеофайлу с таким именем.
//
// Связь — только по имени без расширения, без учёта регистра: «s01e01 - The
// Pilot.mkv» находит «s01e01 - The Pilot.srt» в любом подкаталоге пака.
// Ничего умнее (разбор номеров серий, сопоставление по длительности) здесь
// намеренно нет: пак, разложенный не один в один, лучше не угадывать —
// перепутанные субтитры хуже отсутствующих.
func (l *Library) TracksFor(videoName string) []media.SubtitleTrack {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	return l.byEpisode[episodeKey(videoName)]
}

// Count — сколько файлов субтитров нашлось. Только для журнала при старте.
func (l *Library) Count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
	n := 0
	for _, tracks := range l.byEpisode {
		n += len(tracks)
	}
	return n
}

func (l *Library) refreshLocked() {
	if l.byEpisode != nil && l.now().Sub(l.scanned) < rescanAfter {
		return
	}
	l.scanned = l.now()

	found := make(map[string][]media.SubtitleTrack)
	err := filepath.WalkDir(l.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Один нечитаемый подкаталог не повод потерять весь пак.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !extensions[ext] {
			return nil
		}
		rel, relErr := filepath.Rel(l.dir, path)
		if relErr != nil {
			return nil
		}
		// Имя самого каталога библиотеки идёт в разбор последним сегментом:
		// SUBS_DIR вполне могут нацелить прямо на пак («…/frnds_sub_rus_s01-s10»),
		// и тогда язык больше взять неоткуда. Каталог, названный нейтрально
		// («subs»), просто не совпадёт ни с одним языком.
		key, code, label := describe(filepath.Join(filepath.Base(l.dir), rel))
		found[key] = append(found[key], media.SubtitleTrack{
			// Index — номер потока для `-map 0:N`, а такого потока нет.
			// -1 не сигнальное значение «ошибка», а честное «в файле его нет»;
			// проверяют всё равно не его, а SourcePath.
			Index:      -1,
			Code:       code,
			Label:      label,
			Codec:      strings.TrimPrefix(ext, "."),
			SourcePath: path,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		log.Printf("субтитры %s: %v", l.dir, err)
	}

	// Порядок обязан быть устойчивым: от него зависят коды дорожек
	// (при двух русских паках второй станет rus-2), а телевизор хранит
	// выбранный код в localStorage.
	for _, tracks := range found {
		sort.Slice(tracks, func(i, j int) bool { return tracks[i].SourcePath < tracks[j].SourcePath })
	}
	l.byEpisode = found
}

// describe достаёт из относительного пути ключ серии и язык.
func describe(rel string) (key, code, label string) {
	dir, file := filepath.Split(rel)
	base := strings.TrimSuffix(file, filepath.Ext(file))

	// Хвост вида «.ru» / «.rus» в имени файла — самое точное указание языка,
	// какое бывает: «S01E01.rus.srt» рядом с «S01E01.eng.srt».
	if dot := strings.LastIndex(base, "."); dot > 0 {
		if c, l, ok := media.LanguageFor(base[dot+1:]); ok {
			return strings.ToLower(base[:dot]), c, l
		}
	}
	if c, l, ok := languageFromDirs(dir); ok {
		return strings.ToLower(base), c, l
	}
	// Язык не назван нигде. Дорожка всё равно должна быть выбираемой,
	// поэтому у неё честный код «und» и подпись без вранья про язык.
	return strings.ToLower(base), "und", "External subtitles"
}

// languageFromDirs ищет язык в названиях каталогов, от ближнего к дальнему.
//
// Имя самого файла здесь НЕ участвует: «The One with the English Muffin»
// объявило бы себя английскими субтитрами. У каталога такой беды нет —
// его называют «rus», «Russian» или «frnds_sub_rus_s01-s10», и это ровно
// то, чем он является.
func languageFromDirs(dir string) (code, label string, ok bool) {
	segments := strings.Split(filepath.ToSlash(strings.TrimSuffix(dir, string(filepath.Separator))), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		// Разбиение по не-буквам: «frnds_sub_rus_s01-s10» это токен «rus»
		// плюс мусор, а целиком по нему \brus\b не совпадёт — подчёркивание
		// в регулярках считается частью слова.
		for _, token := range strings.FieldsFunc(segments[i], func(r rune) bool {
			return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' ||
				'а' <= r && r <= 'я' || 'А' <= r && r <= 'Я' || r == 'ё' || r == 'Ё')
		}) {
			if c, l, found := media.LanguageFor(token); found {
				return c, l, true
			}
		}
	}
	return "", "", false
}

// episodeKey — имя видеофайла, приведённое к ключу поиска.
func episodeKey(videoName string) string {
	return strings.ToLower(strings.TrimSuffix(videoName, filepath.Ext(videoName)))
}
