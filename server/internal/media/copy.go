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

// CanCopyAudio — canCopyAudio(track, start), но ШИРЕ эталона: копируется любой
// AAC на 44.1/48 кГц, а не только AAC-LC до двух каналов.
//
// Расхождение с Node осознанное и проверено на телевизоре 17.08.2026.
// В эталоне (и у нас до этого дня) whitelist означал «ровно то, во что мы
// иначе перекодировали бы сами», и всё, что шире, честно перекодировалось.
// Для «Друзей» это значило перекодировать звук каждой серии: английская
// дорожка там HE-AAC 5.1 48 кГц.
//
// Сомнение было не теоретическое. MPEG-TS несёт AAC в ADTS, а в заголовке
// ADTS нет места под явную сигнализацию SBR. В MKV s03e18 лежит ASC
// 13 30 56 e5 9d 48 00 — AOT=2, ядро 24 кГц, syncExtension 0x2B7, extAOT=5
// (SBR), расширение 48 кГц; после `-c:a copy` в TS остаётся заголовок
// ff f1 59 80, то есть profile=AAC-LC, sampling_frequency_index=6 (24000),
// channel_config=6 (5.1). SBR из явной становится подразумеваемой, и декодер,
// который верит заголовку и не разбирает нагрузку, отыграл бы дорожку вдвое
// ниже и вдвое медленнее. Второй риск — 5.1 там, где мы всегда отдавали
// стерео: сводит его телевизор, а диалог живёт в центральном канале.
//
// Проверено ровно на этом: s03e19, та же дорожка (HE-AAC 5.1 48 кГц,
// подразумеваемая SBR), скорость и диалог на слух в порядке. Значит эта
// прошивка и SBR из нагрузки достаёт, и 5.1 сводит сама.
//
// Что этим НЕ проверено: HE-AAC v2 (parametric stereo) и больше шести каналов.
// Они сюда теперь тоже проходят. Если однажды звук поедет — слушать надо
// именно эти два признака, а аварийный выключатель на всё копирование
// целиком — HLS_ALLOW_COPY=0.
//
// Выигрыш измерен на s03e18: 600 с материала — 19.4 с CPU с перекодированием
// звука против 0.29 с с копированием (видео копируется в обоих случаях).
func CanCopyAudio(t *AudioTrack, start float64, allowCopy bool) bool {
	if !allowCopy || t == nil {
		return false
	}
	if start > seekForcesTranscode {
		return false
	}
	if t.Codec != "aac" {
		return false
	}
	// Нулевые каналы — это не «моно», а «ffprobe не разобрал дорожку».
	// Копировать неразобранное не стоит: проверок профиля и числа каналов
	// больше нет, и эта осталась единственной защитой от мусора в метаданных.
	if t.Channels == 0 {
		return false
	}
	return t.SampleRate == 44100 || t.SampleRate == 48000
}
