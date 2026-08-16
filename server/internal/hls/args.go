// Package hls отвечает за сеанс перекодирования: сборку аргументов ffmpeg,
// запуск процесса, подсчёт готовых сегментов и остановку.
package hls

import (
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/media"
)

// seekThreshold — тот же порог, что и в media: ниже него start считается нулевым
// и -ss в аргументы не попадает вовсе.
const seekThreshold = 0.05

// disambiguationSuffix снимает нашу внутреннюю добавку уникальности ('eng-2'),
// потому что в тег языка HLS должен уйти чистый код.
var disambiguationSuffix = regexp.MustCompile(`-\d+$`)

// Params — всё, что нужно для сборки командной строки. Ни файловой системы,
// ни процессов: BuildArgs остаётся чистой функцией, чтобы её можно было
// сверять с эталоном таблицей.
type Params struct {
	// RawURL — http://127.0.0.1:<PORT>/raw/<index>. ffmpeg читает торрент
	// петлёй через наш же HTTP-сервер, и заменять это на pipe нельзя:
	// на каждой перемотке ffmpeg рвёт ответ и делает новый GET с новым Range.
	RawURL     string
	Dir        string
	VideoIndex int
	Audio      *media.AudioTrack
	Subtitle   *media.SubtitleTrack
	Start      float64
	CopyVideo  bool
	CopyAudio  bool
}

// BuildArgs собирает argv для ffmpeg.
//
// Порядок аргументов воспроизводит server.mjs:486-541 дословно и проверяется
// golden-таблицей в args_test.go, снятой с настоящего Node-кода.
func BuildArgs(p Params) []string {
	args := []string{"-hide_banner", "-loglevel", "warning", "-i", p.RawURL}

	// -ss ПОСЛЕ -i — это output-side seek, и так задумано: иначе разъезжаются
	// тайминги встроенных субтитров при продолжении с середины.
	if p.Start > seekThreshold {
		args = append(args, "-ss", jscompat.ToFixed(p.Start, 3))
	}

	args = append(args, "-map", "0:"+strconv.Itoa(p.VideoIndex))
	if p.Audio != nil {
		args = append(args, "-map", "0:"+strconv.Itoa(p.Audio.Index))
	} else {
		args = append(args, "-an")
	}

	// Субтитры НЕ вжигаются фильтром subtitles=: он открыл бы файл второй раз
	// и дочитал субтитры до EOF, а значит торрент скачался бы почти целиком
	// до первого сегмента. Дорожка мапится один раз и уходит отдельным WebVTT.
	if p.Subtitle != nil {
		args = append(args, "-map", "0:"+strconv.Itoa(p.Subtitle.Index))
	}

	args = append(args, videoArgs(p.CopyVideo)...)
	if p.Audio != nil {
		args = append(args, audioArgs(p.CopyAudio)...)
	}

	if p.Subtitle != nil {
		args = append(args, "-c:s", "webvtt")
	} else {
		args = append(args, "-sn")
	}

	args = append(args,
		"-map_metadata", "-1",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_segment_type", "mpegts",
		// temp_file делает появление сегмента атомарным — на это опирается
		// инкрементальный подсчёт готовности в monitor.go.
		"-hls_flags", "temp_file",
		"-hls_segment_filename", filepath.Join(p.Dir, "seg%05d.ts"),
	)

	// -var_stream_map нужен ровно для одного побочного эффекта: он заставляет
	// ffmpeg вынести субтитры отдельной дорожкой WebVTT (index_vtt.m3u8),
	// которую приложение забирает само. Появляющийся при этом master.m3u8
	// не читает никто: Tizen 2.3 не разбирает #EXT-X-MEDIA.
	if p.Subtitle != nil {
		lang := disambiguationSuffix.ReplaceAllString(p.Subtitle.Code, "")
		streams := "v:0"
		if p.Audio != nil {
			streams += ",a:0"
		}
		args = append(args,
			"-var_stream_map", streams+",s:0,sgroup:subs,language:"+lang+",default:yes",
			"-master_pl_name", "master.m3u8",
		)
	}

	return append(args, filepath.Join(p.Dir, "index.m3u8"))
}

func videoArgs(copy bool) []string {
	if copy {
		return []string{"-c:v", "copy"}
	}
	return []string{
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-profile:v", "high",
		"-level:v", "4.0",
		"-sc_threshold", "0",
		// Ключевой кадр раз в 4 с — ровно под -hls_time 4, чтобы сегменты
		// резались одинаковыми. В режиме copy этого рычага нет.
		"-force_key_frames", "expr:gte(t,n_forced*4)",
	}
}

func audioArgs(copy bool) []string {
	if copy {
		return []string{"-c:a", "copy"}
	}
	return []string{"-c:a", "aac", "-b:a", "160k", "-ac", "2", "-ar", "48000"}
}
