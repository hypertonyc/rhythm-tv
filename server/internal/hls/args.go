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

// PlaylistName — единственный плейлист, который открывает плеер. Имя задаём мы
// сами (последний аргумент ffmpeg), и httpapi дописывает в него EXT-X-START
// на отдаче, поэтому имя вынесено из литералов: разъехавшись, они превратили бы
// правку плейлиста в тихий no-op.
const PlaylistName = "index.m3u8"

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
	// Внешняя дорожка для ffmpeg не существует: её нет во входном файле,
	// и мапить нечего. Вторым входом её тоже не подсунуть — при output-side
	// -ss ffmpeg выбрасывает ранние реплики, но оставшимся время
	// не пересчитывает, и при продолжении с середины они уезжают ровно
	// на start. WebVTT для таких дорожек собирает subs.WriteSession,
	// а здесь остаётся честный -sn.
	if p.Subtitle.External() {
		p.Subtitle = nil
	}

	args := []string{"-hide_banner", "-loglevel", "warning"}

	// Переподключение к входу. У ffmpeg все эти флаги по умолчанию ВЫКЛЮЧЕНЫ:
	// любой обрыв до EOF — и процесс выходит, не пытаясь снова.
	//
	// Для торрента это неверное поведение. Данные тянет только живой Reader,
	// заранее ничего не качается, и на файл без роя чтение через /raw встаёт
	// или рвётся — например, первую минуту после перезапуска, пока клиент
	// набирает пиров. Без переподключения такой сеанс умирал сразу и уходил
	// в state=error, хотя через минуту всё заработало бы само.
	//
	// Это осознанное расхождение с Node-эталоном: там тех же флагов нет,
	// и там была та же слабость. Golden-тест сверяет остальные аргументы,
	// вырезая этот префикс (см. args_test.go).
	args = append(args,
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_on_network_error", "1",
		// Потолок паузы между попытками. Умолчание 120 с слишком велико:
		// телевизор ждёт сегментов и опрашивает статус каждые 700 мс.
		"-reconnect_delay_max", "30",
	)

	args = append(args, "-i", p.RawURL)

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
		// #EXT-X-PLAYLIST-TYPE:EVENT — «плейлист только дописывается».
		//
		// Без него плейлист выглядит как обычный live, и плеер по правилу
		// HLS (RFC 8216, 6.3.3) входит не в начало, а за три TARGETDURATION
		// от конца. Для нас это не теория: ffmpeg в режиме copy опережает
		// реальное время в ~15 раз, поэтому к моменту, когда AVPlay доберётся
		// до плейлиста (у него на closeAvplay + prepareAsync уходят секунды),
		// на диске лежит уже минута сеанса — и телевизор начинал серию
		// с 00:15-00:35. В логе nginx это видно однозначно: сеанс запущен
		// в 11:58:04, плейлист забран в 11:58:06, первый запрошенный сегмент —
		// seg00008, а он начинается на 33.6 с.
		//
		// Осознанное расхождение с Node-эталоном: там его тоже не было
		// и там была та же болезнь. Golden-тест вырезает этот флаг и сверяет
		// остальное побайтово. ENDLIST в конце EVENT не отменяет (проверено
		// на ffmpeg 5.1 из прод-образа), поэтому конец серии по-прежнему виден.
		"-hls_playlist_type", "event",
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

	return append(args, filepath.Join(p.Dir, PlaylistName))
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
