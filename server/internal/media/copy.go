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
func CanCopyAudio(t *AudioTrack, start float64, allowCopy bool) bool {
	if !allowCopy || t == nil {
		return false
	}
	if start > seekForcesTranscode {
		return false
	}
	if t.Codec != "aac" || strings.ToUpper(t.Profile) != "LC" {
		return false
	}
	if t.Channels == 0 || t.Channels > 2 {
		return false
	}
	return t.SampleRate == 44100 || t.SampleRate == 48000
}
