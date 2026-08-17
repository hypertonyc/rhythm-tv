package hls

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/media"
)

// Pipeline — из чего во что перекодируется сеанс, готовыми строками.
//
// Строками, а не полями: единственный читатель — страница на телефоне,
// и раскладывать там форматирование заново значило бы держать вторую копию
// правил «level 40 это 4.0», «2 канала это stereo». Правила эти живут в одном
// месте — здесь, рядом с теми, что решают, копировать дорожку или нет.
type Pipeline struct {
	Video PipelineTrack  `json:"video"`
	Audio *PipelineTrack `json:"audio"` // null, если дорожки звука нет вовсе
}

// PipelineTrack — одна дорожка. To при Mode == "copy" равен From не случайно:
// копирование это и есть «на выходе ровно то же самое», и показать это надо
// так же явно, как перекодирование.
type PipelineTrack struct {
	Mode string `json:"mode"` // copy | transcode
	From string `json:"from"`
	To   string `json:"to"`
}

// describePipeline собирает описание по тем же данным, по которым принималось
// решение о копировании, — иначе описание и дело разъедутся молча.
func describePipeline(v *media.VideoInfo, a *media.AudioTrack, copyVideo, copyAudio bool) Pipeline {
	p := Pipeline{Video: PipelineTrack{Mode: transcodeMode(copyVideo), From: describeVideo(v)}}
	if copyVideo {
		p.Video.To = p.Video.From
	} else {
		// Ровно то, что стоит в videoArgs. Разрешение не меняется: фильтра
		// масштабирования у нас нет вовсе.
		p.Video.To = joinParts("h264", "High@4.0", "yuv420p", resolution(v), "libx264 crf 20")
	}

	if a == nil {
		return p
	}
	audio := PipelineTrack{Mode: transcodeMode(copyAudio), From: describeAudio(a)}
	if copyAudio {
		audio.To = audio.From
	} else {
		// Ровно то, что стоит в audioArgs.
		audio.To = joinParts("aac", "LC", "stereo", "48.0 kHz", "160k")
	}
	p.Audio = &audio
	return p
}

func transcodeMode(isCopy bool) string {
	if isCopy {
		return "copy"
	}
	return "transcode"
}

func describeVideo(v *media.VideoInfo) string {
	if v == nil {
		return "no video"
	}
	return joinParts(v.Codec, videoProfile(v), v.PixFmt, resolution(v))
}

func describeAudio(a *media.AudioTrack) string {
	if a == nil {
		return "no audio"
	}
	return joinParts(a.Codec, a.Profile, channels(a.Channels), sampleRate(a.SampleRate))
}

// videoProfile склеивает профиль с уровнем: `High@4.0`. Уровень у ffprobe —
// целое в десятых (41 это 4.1), а -99 означает «не разобрал» и показывать
// его человеку незачем.
func videoProfile(v *media.VideoInfo) string {
	if v.Profile == "" {
		return ""
	}
	if v.Level <= 0 {
		return v.Profile
	}
	return fmt.Sprintf("%s@%d.%d", v.Profile, v.Level/10, v.Level%10)
}

func resolution(v *media.VideoInfo) string {
	if v == nil || v.Width <= 0 || v.Height <= 0 {
		return ""
	}
	return strconv.Itoa(v.Width) + "x" + strconv.Itoa(v.Height)
}

// channels: цифра сама по себе человеку ничего не говорит, а «5.1» говорит.
func channels(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "mono"
	case n == 2:
		return "stereo"
	case n == 6:
		return "5.1"
	case n == 8:
		return "7.1"
	default:
		return strconv.Itoa(n) + "ch"
	}
}

func sampleRate(hz int) string {
	if hz <= 0 {
		return ""
	}
	// ToFixed, а не FormatFloat: у Go округление половин к чётному,
	// и 44.05 кГц показалось бы иначе, чем везде на сервере.
	return jscompat.ToFixed(float64(hz)/1000, 1) + " kHz"
}

// joinParts выбрасывает пустые куски: у рипов сплошь и рядом нет то профиля,
// то частоты, и «h264,  , yuv420p» выглядело бы как поломка страницы.
func joinParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}
