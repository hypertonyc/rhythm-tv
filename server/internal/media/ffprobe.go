package media

import (
	"encoding/json"
	"fmt"

	"github.com/avdav/torrent-media/server/internal/jscompat"
)

// probeOutput — то, что печатает `ffprobe -show_streams -show_format -of json`.
//
// Поля намеренно взяты как any: ffprobe непоследователен в типах (width это
// число, sample_rate — строка), а Node пропускал их через Number(x) || 0,
// который принимает оба варианта. jscompat.NumberAnyOr0 делает то же самое.
type probeOutput struct {
	Streams []probeStream `json:"streams"`
	Format  *struct {
		Duration any `json:"duration"`
	} `json:"format"`
}

type probeStream struct {
	Index       int            `json:"index"`
	CodecType   string         `json:"codec_type"`
	CodecName   string         `json:"codec_name"`
	Profile     any            `json:"profile"`
	Width       any            `json:"width"`
	Height      any            `json:"height"`
	PixFmt      string         `json:"pix_fmt"`
	Level       any            `json:"level"`
	FieldOrder  string         `json:"field_order"`
	Channels    any            `json:"channels"`
	SampleRate  any            `json:"sample_rate"`
	Tags        map[string]any `json:"tags"`
	Disposition map[string]any `json:"disposition"`
}

// ParseProbe собирает Result из вывода ffprobe.
//
// Функция чистая: ни процессов, ни сети, ни файловой системы. Индекс, имя
// и соседние серии приходят снаружи, потому что их знает только слой торрента.
func ParseProbe(raw []byte, index int, name string, next, prev *int) (*Result, error) {
	var out probeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}

	// Слайсы создаются непустыми намеренно: nil-слайс уедет в JSON как null,
	// а клиент читает meta.audio.length без проверки и упадёт целиком.
	audio := make([]AudioTrack, 0, len(out.Streams))
	subtitles := make([]SubtitleTrack, 0, len(out.Streams))
	var video *VideoInfo

	for _, s := range out.Streams {
		switch s.CodecType {
		case "audio":
			ordinal := len(audio)
			code, label := NormalizeLanguage(s.Tags, ordinal, "audio")
			audio = append(audio, AudioTrack{
				Index:         s.Index,
				RelativeIndex: ordinal,
				Code:          code,
				Label:         label,
				Codec:         s.CodecName,
				Profile:       stringOr(s.Profile),
				Channels:      int(jscompat.NumberAnyOr0(s.Channels)),
				SampleRate:    int(jscompat.NumberAnyOr0(s.SampleRate)),
				Default:       dispositionDefault(s.Disposition),
			})
		case "subtitle":
			ordinal := len(subtitles)
			code, label := NormalizeLanguage(s.Tags, ordinal, "sub")
			subtitles = append(subtitles, SubtitleTrack{
				Index:         s.Index,
				RelativeIndex: ordinal,
				Code:          code,
				Label:         label,
				Codec:         s.CodecName,
				Default:       dispositionDefault(s.Disposition),
			})
		case "video":
			// Берётся первый видеопоток и только он: обложка-превью в MKV
			// приходит вторым видеопотоком, и мапить надо не её.
			if video != nil {
				continue
			}
			video = &VideoInfo{
				Index:      s.Index,
				Codec:      s.CodecName,
				Width:      int(jscompat.NumberAnyOr0(s.Width)),
				Height:     int(jscompat.NumberAnyOr0(s.Height)),
				PixFmt:     s.PixFmt,
				Profile:    stringOr(s.Profile),
				Level:      int(jscompat.NumberAnyOr0(s.Level)),
				FieldOrder: s.FieldOrder,
			}
		}
	}

	DisambiguateAll(audio, subtitles)

	var duration float64
	if out.Format != nil {
		duration = jscompat.NumberAnyOr0(out.Format.Duration)
	}

	return &Result{
		Index:     index,
		Name:      name,
		Duration:  duration,
		Video:     video,
		Audio:     audio,
		Subtitles: subtitles,
		Next:      next,
		Prev:      prev,
	}, nil
}

// DisambiguateAll — два НЕЗАВИСИМЫХ прохода, как в server.mjs:212 и :229.
// Общий проход был бы ошибкой: субтитровая дорожка 'rus' не должна становиться
// 'rus-2' только потому, что аудиодорожка 'rus' уже есть.
func DisambiguateAll(audio []AudioTrack, subtitles []SubtitleTrack) {
	var ac, al []*string
	for i := range audio {
		ac = append(ac, &audio[i].Code)
		al = append(al, &audio[i].Label)
	}
	Disambiguate(ac, al)

	var sc, sl []*string
	for i := range subtitles {
		sc = append(sc, &subtitles[i].Code)
		sl = append(sl, &subtitles[i].Label)
	}
	Disambiguate(sc, sl)
}

// stringOr — `s.profile || ""`.
func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// dispositionDefault — Boolean(s.disposition && s.disposition.default).
// ffprobe пишет сюда 0 или 1, а не true/false.
func dispositionDefault(d map[string]any) bool {
	if d == nil {
		return false
	}
	v, ok := d["default"]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	default:
		return false
	}
}
