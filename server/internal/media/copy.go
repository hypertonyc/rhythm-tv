package media

import "strings"

// Ремукс без перекодирования ловит форматы, которых декодер 2015 года
// не понимает, поэтому whitelist узкий: ровно то, во что мы иначе
// перекодировали бы сами. Всё остальное (10 бит, 4:2:2, 4K, level 5+,
// чересстрочное) по-прежнему идёт через libx264.
var h264SafeProfiles = map[string]bool{
	"constrained baseline": true,
	"baseline":             true,
	"main":                 true,
	"high":                 true,
}

// seekForcesTranscode — порог, ниже которого start считается нулевым.
//
// Output-side seek режет поток не по ключевому кадру, а вставить свои
// ключевые кадры в копируемый поток нельзя — при перемотке только
// перекодирование. Тот же порог гасит и копирование звука.
const seekForcesTranscode = 0.05

// CanCopyVideo — canCopyVideo(video, start).
func CanCopyVideo(v *VideoInfo, start float64, allowCopy bool) bool {
	if !allowCopy || v == nil {
		return false
	}
	if start > seekForcesTranscode {
		return false
	}
	if v.Codec != "h264" || v.PixFmt != "yuv420p" {
		return false
	}
	if !h264SafeProfiles[strings.ToLower(v.Profile)] {
		return false
	}
	// Проверка воспроизводит `!video.level || video.level > 41` дословно,
	// включая её дырку: ffprobe отдаёт level:-99 для неизвестного уровня,
	// и -99 проходит оба условия. Баг унаследован сознательно —
	// контракт заморожен, чинить его надо отдельным решением.
	if v.Level == 0 || v.Level > 41 {
		return false
	}
	if v.Width > 1920 || v.Height > 1088 {
		return false
	}
	if v.FieldOrder != "" && v.FieldOrder != "progressive" {
		return false
	}
	return true
}

// CanCopyAudio — canCopyAudio(track, start).
//
// anyAAC (HLS_AUDIO_COPY_ANY_AAC) снимает две проверки из четырёх — профиль
// и число каналов, — оставляя кодек и частоту. Это ЭКСПЕРИМЕНТАЛЬНЫЙ рычаг,
// по умолчанию выключенный, и вот чем он рискует.
//
// Узкий whitelist — это «ровно то, во что мы иначе перекодировали бы сами»
// (AAC-LC стерео 48 кГц). Расширить его значит утверждать что-то про декодер
// телевизора, а не про наш ffmpeg, и проверить это можно только на телевизоре.
// Для HE-AAC у сомнения есть измеренное основание: MPEG-TS несёт AAC в ADTS,
// а в заголовке ADTS нет места под явную сигнализацию SBR. У «Друзей» s03e18
// (eng, HE-AAC 5.1 48 кГц) в MKV лежит ASC 13 30 56 e5 9d 48 00 — AOT=2,
// ядро 24 кГц, syncExtension 0x2B7, extAOT=5 (SBR), расширение 48 кГц;
// после `-c:a copy` в TS получается заголовок ff f1 59 80, то есть
// profile=AAC-LC, sampling_frequency_index=6 (24000), channel_config=6 (5.1).
// SBR из явной становится подразумеваемой: декодер, который верит заголовку
// и не разбирает полезную нагрузку, отыграет дорожку на 24 кГц — вдвое ниже
// и вдвое медленнее. Плюс 5.1 там, где мы всегда отдавали стерео: сводить
// в две колонки будет телевизор, а не мы, и диалог живёт в центральном канале.
//
// Ошибка здесь не падает и не логируется — она слышна. Поэтому рычаг отдельный
// и выключен: включать его стоит на один сеанс, слушать своими ушами и,
// если всё хорошо, делать умолчанием, убрав его совсем.
func CanCopyAudio(t *AudioTrack, start float64, allowCopy, anyAAC bool) bool {
	if !allowCopy || t == nil {
		return false
	}
	if start > seekForcesTranscode {
		return false
	}
	if t.Codec != "aac" {
		return false
	}
	// Нулевые каналы — это не «моно и не стерео», а «ffprobe не разобрал
	// дорожку». Копировать неразобранное не стоит и в широком режиме.
	if t.Channels == 0 {
		return false
	}
	if !anyAAC {
		if strings.ToUpper(t.Profile) != "LC" {
			return false
		}
		if t.Channels > 2 {
			return false
		}
	}
	return t.SampleRate == 44100 || t.SampleRate == 48000
}
