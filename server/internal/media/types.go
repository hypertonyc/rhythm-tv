// Package media отвечает за разбор дорожек файла и за решение, можно ли отдать
// их без перекодирования. Формат структур ниже — часть замороженного контракта
// с телевизором: порядок полей, их типы и то, какие из них бывают null,
// повторяют объектные литералы из legacy/server.mjs дословно.
package media

// Result — ответ /api/probe/:index.
//
// Порядок полей = порядок в литерале Node (server.mjs:243-261). encoding/json
// сериализует в порядке объявления, так что перестановка полей здесь — это
// перестановка байтов в ответе.
type Result struct {
	Index     int             `json:"index"`
	Name      string          `json:"name"`
	Duration  float64         `json:"duration"`
	Video     *VideoInfo      `json:"video"`
	Audio     []AudioTrack    `json:"audio"`
	Subtitles []SubtitleTrack `json:"subtitles"`
	// Next и Prev обязаны быть именно null, а не отсутствовать: клиент
	// сравнивает `meta.next === null` строго (app.js:944), а undefined
	// этой проверки не проходит — и «следующая серия» запустилась бы
	// на последней.
	Next *int `json:"next"`
	Prev *int `json:"prev"`
}

// VideoInfo — первый видеопоток файла. null, если видео нет вовсе.
type VideoInfo struct {
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	PixFmt     string `json:"pixFmt"`
	Profile    string `json:"profile"`
	Level      int    `json:"level"`
	FieldOrder string `json:"fieldOrder"`
}

// AudioTrack — Index это абсолютный номер потока для `-map 0:N`,
// RelativeIndex — порядковый среди аудиодорожек.
type AudioTrack struct {
	Index         int    `json:"index"`
	RelativeIndex int    `json:"relativeIndex"`
	Code          string `json:"code"`
	Label         string `json:"label"`
	Codec         string `json:"codec"`
	Profile       string `json:"profile"`
	Channels      int    `json:"channels"`
	SampleRate    int    `json:"sampleRate"`
	Default       bool   `json:"default"`
}

// SubtitleTrack — то же для субтитров; profile/channels/sampleRate тут не нужны,
// потому что решения «копировать или нет» для субтитров не существует.
type SubtitleTrack struct {
	Index         int    `json:"index"`
	RelativeIndex int    `json:"relativeIndex"`
	Code          string `json:"code"`
	Label         string `json:"label"`
	Codec         string `json:"codec"`
	Default       bool   `json:"default"`

	// SourcePath — путь к отдельному файлу субтитров, если дорожка взята
	// не из самой серии (см. internal/subs). Пустая строка означает обычный
	// поток внутри файла, и тогда Index — его номер для `-map 0:N`.
	//
	// В JSON поля НЕТ намеренно: ответ /api/probe сверяется с Node-эталоном
	// побайтово, а телевизору путь на сервере не нужен и знать он о нём
	// не должен. Дорожка отличается для него только кодом и подписью.
	SourcePath string `json:"-"`
}

// External — дорожка лежит отдельным файлом, а не потоком внутри серии.
func (t *SubtitleTrack) External() bool { return t != nil && t.SourcePath != "" }
