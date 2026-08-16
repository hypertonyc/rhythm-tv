package media

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/avdav/torrent-media/server/internal/jscompat"
)

// language — одна запись таблицы языков.
//
// Латиница и кириллица проверяются РАЗНЫМИ шаблонами, и это надо сохранить
// дословно. В JS `\b` определён через [A-Za-z0-9_], поэтому вокруг кириллических
// букв границы слова не существует и /\bрус\b/ не совпадает ни с чем никогда.
// В Go RE2 `\b` устроен ровно так же, так что «упростить» это в один шаблон
// с (?i) нельзя: поведение изменится в обе стороны сразу.
type language struct {
	code     string
	label    string
	latin    *regexp.Regexp
	cyrillic *regexp.Regexp
}

var languages = []language{
	{"rus", "Russian", regexp.MustCompile(`\b(rus|ru|russian)\b`), regexp.MustCompile(`рус`)},
	{"eng", "English", regexp.MustCompile(`\b(eng|en|english)\b`), regexp.MustCompile(`англ`)},
	{"tha", "Thai", regexp.MustCompile(`\b(tha|th|thai)\b`), regexp.MustCompile(`тай`)},
}

// videoName — фильтр списка серий, /\.(mp4|m4v|mkv|webm)$/i из server.mjs:71.
//
// Классы расписаны по буквам намеренно: (?i) в Go делает юникодный fold,
// и (?i)mkv совпал бы с «m<KELVIN SIGN>v», а JS-овый /i без /u — нет.
var videoName = regexp.MustCompile(`\.([Mm][Pp]4|[Mm]4[Vv]|[Mm][Kk][Vv]|[Ww][Ee][Bb][Mm])$`)

// IsVideoName повторяет фильтр videoFiles(): в меню попадают только эти расширения.
func IsVideoName(name string) bool { return videoName.MatchString(name) }

// TrackTitle — trackTitle(tags): схлопнуть пробелы, обрезать края и,
// если получилось длиннее 40 символов, укоротить с многоточием.
func TrackTitle(tags map[string]any) string {
	title := jscompat.TrimJS(jscompat.CollapseWhitespace(tagString(tags, "title")))
	return jscompat.TruncateUTF16(title, 40, "…")
}

// NormalizeLanguage — normalizeLanguage(stream, ordinal, kind).
// kind это "audio" или "sub"; он же уходит в запасной код вида "audio-1".
func NormalizeLanguage(tags map[string]any, ordinal int, kind string) (code, label string) {
	raw := strings.ToLower(tagString(tags, "language"))
	title := TrackTitle(tags)
	haystack := raw + " " + strings.ToLower(title)

	for _, lang := range languages {
		if !lang.latin.MatchString(haystack) && !lang.cyrillic.MatchString(haystack) {
			continue
		}
		// Название дорожки идёт в подпись: у рипов с двумя дорожками одного
		// языка это единственное, чем «Дубляж» отличается от «Войсовер» в меню.
		if title != "" {
			return lang.code, lang.label + " — " + title
		}
		return lang.code, lang.label
	}

	if raw != "" && raw != "und" {
		if title != "" {
			return raw, title
		}
		return raw, strings.ToUpper(raw)
	}

	fallback := "Subtitles"
	if kind == "audio" {
		fallback = "Audio"
	}
	if title != "" {
		return fmt.Sprintf("%s-%d", kind, ordinal+1), title
	}
	return fmt.Sprintf("%s-%d", kind, ordinal+1), fmt.Sprintf("%s %d", fallback, ordinal+1)
}

// LanguageFor опознаёт язык по одному слову — названию каталога или суффиксу
// имени файла с субтитрами (см. internal/subs).
//
// Таблица здесь та же, что у дорожек внутри файла, и это главное: каталог
// «rus» обязан дать код rus, тот же самый, что встроенная русская дорожка,
// иначе сохранённый на телевизоре выбор перестал бы совпадать.
func LanguageFor(token string) (code, label string, ok bool) {
	token = strings.ToLower(token)
	for _, lang := range languages {
		if lang.latin.MatchString(token) || lang.cyrillic.MatchString(token) {
			return lang.code, lang.label, true
		}
	}
	return "", "", false
}

// AttachExternal дописывает к разбору дорожки, которых в файле нет.
//
// Вызывается ПОСЛЕ ParseProbe, потому что про файлы рядом с сервером знает
// не ffprobe, а вызывающий. Дизамбигуация прогоняется заново по общему
// списку: внешний «rus» рядом со встроенным «rus» обязан стать «rus-2»,
// иначе ChooseTrack нашёл бы первый попавшийся и вторая дорожка была бы
// недостижима. Повторный прогон безопасен — на уже уникальном списке
// Disambiguate ничего не меняет.
func AttachExternal(r *Result, tracks []SubtitleTrack) {
	if r == nil || len(tracks) == 0 {
		return
	}
	for _, t := range tracks {
		t.Index = -1
		t.RelativeIndex = len(r.Subtitles)
		t.Default = false
		r.Subtitles = append(r.Subtitles, t)
	}
	DisambiguateAll(r.Audio, r.Subtitles)
}

// Disambiguate — disambiguateTracks(): коды и подписи обязаны быть уникальными.
//
// ChooseTrack ищет дорожку по коду, и при двух русских дорожках (дубляж +
// войсовер — обычное дело для рипов) вторая была бы недостижима. Первая
// сохраняет чистый код, чтобы сохранённое у клиента предпочтение 'rus'
// продолжало работать; последующие получают -2, -3 и так далее.
//
// Работает по указателям в поля самих дорожек, потому что аудио и субтитры —
// разные типы, а правило одно.
func Disambiguate(codes, labels []*string) {
	usedCodes := make(map[string]bool, len(codes))
	usedLabels := make(map[string]bool, len(labels))

	for i := range codes {
		base := *codes[i]
		code := base
		for seq := 1; usedCodes[code]; {
			seq++
			code = fmt.Sprintf("%s-%d", base, seq)
		}
		usedCodes[code] = true
		*codes[i] = code

		baseLabel := *labels[i]
		label := baseLabel
		for seq := 1; usedLabels[label]; {
			seq++
			label = fmt.Sprintf("%s %d", baseLabel, seq)
		}
		usedLabels[label] = true
		*labels[i] = label
	}
}

// ChooseTrack — chooseTrack(tracks, preference).
//
// Порядок предпочтений: точное совпадение кода, затем первая дорожка
// с disposition.default, затем просто первая. Пустое preference совпадением
// не считается (в JS `if (preference)` отсекает пустую строку).
func ChooseTrack[T any](tracks []T, preference string, id func(*T) (code string, isDefault bool)) *T {
	if len(tracks) == 0 {
		return nil
	}
	if preference != "" {
		for i := range tracks {
			if code, _ := id(&tracks[i]); code == preference {
				return &tracks[i]
			}
		}
	}
	for i := range tracks {
		if _, def := id(&tracks[i]); def {
			return &tracks[i]
		}
	}
	return &tracks[0]
}

// ChooseAudio и ChooseSubtitle — обёртки над ChooseTrack для двух конкретных типов.
func ChooseAudio(tracks []AudioTrack, preference string) *AudioTrack {
	return ChooseTrack(tracks, preference, func(t *AudioTrack) (string, bool) {
		return t.Code, t.Default
	})
}

func ChooseSubtitle(tracks []SubtitleTrack, preference string) *SubtitleTrack {
	return ChooseTrack(tracks, preference, func(t *SubtitleTrack) (string, bool) {
		return t.Code, t.Default
	})
}

// tagString — `String(tags[key] || ”)` для значения, разобранного из JSON.
func tagString(tags map[string]any, key string) string {
	v, ok := tags[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "" // String(false || '') === ''
	case float64:
		if t == 0 {
			return "" // 0 ложно, поэтому `|| ''` подставит пустую строку
		}
		b, err := jscompat.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return ""
	}
}
