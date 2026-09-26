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
	RawURL    string
	Dir       string
	Video     *media.VideoInfo
	Audio     *media.AudioTrack
	Subtitle  *media.SubtitleTrack
	Start     float64
	CopyVideo bool
	CopyAudio bool
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

	// Машинный отчёт о ходе работы в stdout. Нужен ради одной цифры, которой
	// иначе взять негде: сколько секунд видео ffmpeg уже произвёл. Телевизор
	// ждёт два стартовых сегмента и всё это время держит чёрный экран, а по
	// готовым файлам виден только счёт 0/2 → 1/2 → 2/2 — три ступеньки на
	// пятнадцать-двадцать секунд, по которым нельзя ни оценить остаток,
	// ни отличить работу от зависания.
	//
	// stdout до этого выбрасывался, и он свободен: вывод у нас идёт в файлы.
	// Человеческий прогресс в stderr парсить не стоит — он пишется через \r,
	// перемешан с предупреждениями и не обещает формата; `-progress` для того
	// и сделан. Расхождение с эталоном вырезается golden-тестом, как и флаги
	// переподключения (см. args_test.go).
	args = append(args, "-progress", "pipe:1")

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

	// Деблокинг 4K съедает четверть времени декодера, а уменьшение кадра всё равно
	// его размывает: PSNR с ним и без него — 46 дБ.
	if _, _, scaled := outputFrame(p.Video); scaled && !p.CopyVideo {
		args = append(args, "-skip_loop_filter", "all")
	}

	args = append(args, "-i", p.RawURL)

	// -ss ПОСЛЕ -i — это output-side seek, и так задумано: иначе разъезжаются
	// тайминги встроенных субтитров при продолжении с середины.
	if p.Start > seekThreshold {
		args = append(args, "-ss", jscompat.ToFixed(p.Start, 3))
	}

	args = append(args, "-map", "0:"+strconv.Itoa(p.Video.Index))
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

	args = append(args, videoArgs(p.CopyVideo, p.Video)...)
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
		"-hls_time", strconv.Itoa(SegmentSeconds),
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

func videoArgs(copy bool, v *media.VideoInfo) []string {
	if copy {
		return []string{"-c:v", "copy"}
	}
	var args []string
	if filter := videoFilter(v); filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-profile:v", "high",
		"-level:v", "4.0",
		"-sc_threshold", "0",
		// Ключевой кадр раз в 4 с — ровно под -hls_time 4, чтобы сегменты
		// резались одинаковыми. В режиме copy этого рычага нет.
		"-force_key_frames", "expr:gte(t,n_forced*"+strconv.Itoa(SegmentSeconds)+")",
	)
	if v.HDR() {
		// ffmpeg 5.1 берёт метки цвета у декодера, а не у кадра: без них сведённый
		// в SDR поток назывался бы PQ/BT.2020.
		args = append(args, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	}
	return args
}

const (
	frameMaxWidth  = 1920
	frameMaxHeight = 1088
)

// HDR уменьшается сильнее: на проде тонмаппинг в 1080p не успевает за реальным
// временем, а в 720p успевает.
func frameBox(v *media.VideoInfo) (int, int) {
	if v.HDR() {
		return 1280, 720
	}
	return 1920, 1080
}

func outputFrame(v *media.VideoInfo) (w, h int, scaled bool) {
	if v.Width <= 0 || v.Height <= 0 || (v.Width <= frameMaxWidth && v.Height <= frameMaxHeight) {
		return v.Width, v.Height, false
	}
	boxW, boxH := frameBox(v)
	w, h = boxW, roundDiv(v.Height*boxW, v.Width)
	if h > boxH {
		w, h = roundDiv(v.Width*boxH, v.Height), boxH
	}
	return w &^ 1, h &^ 1, true
}

func roundDiv(a, b int) int { return (a + b/2) / b }

// zscale, а не scale: swscale в ffmpeg 5.1 однопоточный и на 4K съедал четверть скорости.
func videoFilter(v *media.VideoInfo) string {
	w, h, scaled := outputFrame(v)
	var size string
	if scaled {
		size = "w=" + strconv.Itoa(w) + ":h=" + strconv.Itoa(h) + ":f=bilinear"
	}
	switch {
	case v.HDR():
		linear := "tin=" + v.ColorTransfer + ":min=bt2020nc:pin=bt2020:rin=tv:t=linear:npl=100"
		if size != "" {
			linear = size + ":" + linear
		}
		// Уменьшение — в первом же проходе, до тонмаппинга: тот считает во float,
		// и каждый лишний пиксель стоит дорого.
		return "zscale=" + linear +
			",format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=mobius:desat=0" +
			",zscale=t=bt709:m=bt709:r=tv,format=yuv420p"
	case scaled:
		return "zscale=" + size + ",format=yuv420p"
	}
	return ""
}

func audioArgs(copy bool) []string {
	if copy {
		return []string{"-c:a", "copy"}
	}
	return []string{"-c:a", "aac", "-b:a", "160k", "-ac", "2", "-ar", "48000"}
}
