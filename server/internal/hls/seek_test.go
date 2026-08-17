package hls

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avdav/torrent-media/server/internal/media"
)

// seekProbe — менеджер с подделкой ffmpeg, которая записывает argv, и с
// подставным поиском ключевого кадра. Возвращает ещё и счётчик обращений
// к поиску: половина смысла выравнивания в том, чтобы НЕ ходить за ключевым
// кадром там, где он не нужен.
func seekProbe(t *testing.T, keyframe float64, found bool) (*Manager, func() string, func() int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("подделка ffmpeg — shell-скрипт")
	}

	tmp := t.TempDir()
	argv := filepath.Join(tmp, "argv")
	shim := filepath.Join(tmp, "ffmpeg-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho \"$@\" > "+argv+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	asked := 0
	m := &Manager{
		TmpDir:    tmp,
		FFmpeg:    shim,
		AllowCopy: true,
		RawURL:    func(int) string { return "http://127.0.0.1:8000/raw/0" },
		NowMilli:  func() int64 { return 1786870000000 },
		Keyframe: func(index, videoIndex int, start float64) (float64, bool) {
			asked++
			return keyframe, found
		},
	}
	t.Cleanup(m.Shutdown)

	// Процесс запускается и ждётся в горутинах, поэтому argv появляется
	// не мгновенно: подделка успевает записать файл через считаные миллисекунды.
	args := func() string {
		t.Helper()
		for i := 0; i < 200; i++ {
			if data, err := os.ReadFile(argv); err == nil {
				return string(data)
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("подделка ffmpeg так и не запустилась")
		return ""
	}
	return m, args, func() int { return asked }
}

func copyableEpisode() *media.Result {
	return &media.Result{
		Index: 0, Name: "s03e23.mkv", Duration: 1368.394,
		Video: &media.VideoInfo{Index: 0, Codec: "h264", Profile: "High", Level: 41,
			PixFmt: "yuv420p", Width: 768, Height: 432, FieldOrder: "progressive"},
		Audio: []media.AudioTrack{{Index: 2, Code: "eng", Codec: "aac",
			Profile: "HE-AAC", Channels: 6, SampleRate: 48000}},
		Subtitles: make([]media.SubtitleTrack, 0),
	}
}

// TestSeekAlignsToKeyframeAndCopies — главный тест перемотки копированием.
//
// Проверяется не «copy включился», а то, что подтянутая секунда ОДНА на весь
// сеанс: она уходит в -ss и она же попадает в снимок. Разъехавшись, эти два
// значения дадут самую неприятную из возможных поломок — картинка идёт,
// ошибок нет, а позиция и внешние субтитры смещены на полGOP.
func TestSeekAlignsToKeyframeAndCopies(t *testing.T) {
	m, args, asked := seekProbe(t, 186.186, true)

	snap, err := m.Start(StartOptions{
		Index: 0, Meta: copyableEpisode(), AudioPref: "eng", SubPref: "off", Start: 195,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 186.186 - keyframeLead: -ss ровно на ключевом кадре его не включает.
	const want = "186.086"
	if snap.Start != 186.086 {
		t.Errorf("снимок отдаёт start %v, ожидалось 186.086", snap.Start)
	}
	line := args()
	if !strings.Contains(line, "-ss "+want) {
		t.Errorf("в argv ушла другая секунда:\n%s", line)
	}
	if strings.Contains(line, "-ss 195") {
		t.Errorf("в argv ушла запрошенная секунда, а не выровненная:\n%s", line)
	}
	if !strings.Contains(line, "-c:v copy") || !strings.Contains(line, "-c:a copy") {
		t.Errorf("выровненная перемотка обязана копироваться:\n%s", line)
	}
	if snap.VideoMode != "copy" || snap.AudioMode != "copy" {
		t.Errorf("снимок описывает %s/%s, ожидалось copy/copy", snap.VideoMode, snap.AudioMode)
	}
	if n := asked(); n != 1 {
		t.Errorf("поиск ключевого кадра звали %d раз, ожидался один", n)
	}
}

// TestSeekWithoutKeyframeFallsBackToTranscode — не нашли ключевой кадр
// (нет пиров, таймаут, ffprobe не понял файл) значит ведём себя как раньше.
func TestSeekWithoutKeyframeFallsBackToTranscode(t *testing.T) {
	m, args, _ := seekProbe(t, 0, false)

	snap, err := m.Start(StartOptions{
		Index: 0, Meta: copyableEpisode(), AudioPref: "eng", SubPref: "off", Start: 195,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Start != 195 {
		t.Errorf("start сдвинулся без ключевого кадра: %v", snap.Start)
	}
	if line := args(); !strings.Contains(line, "-c:v libx264") || !strings.Contains(line, "-ss 195.000") {
		t.Errorf("ожидалось прежнее поведение:\n%s", line)
	}
	if snap.VideoMode != "transcode" {
		t.Errorf("режим %s, ожидался transcode", snap.VideoMode)
	}
}

// TestSeekSkipsKeyframeLookupWhenPointless — ffprobe стоит времени на холодном
// рое, и звать его там, где ответ ничего не изменит, нельзя.
func TestSeekSkipsKeyframeLookupWhenPointless(t *testing.T) {
	t.Run("старт с нуля", func(t *testing.T) {
		m, _, asked := seekProbe(t, 186.186, true)
		if _, err := m.Start(StartOptions{
			Index: 0, Meta: copyableEpisode(), AudioPref: "eng", SubPref: "off",
		}); err != nil {
			t.Fatal(err)
		}
		if asked() != 0 {
			t.Error("на нулевой точке ключевой кадр искать незачем")
		}
	})

	t.Run("формат всё равно перекодируется", func(t *testing.T) {
		m, args, asked := seekProbe(t, 186.186, true)
		meta := copyableEpisode()
		meta.Video.Codec = "hevc"
		if _, err := m.Start(StartOptions{
			Index: 0, Meta: meta, AudioPref: "eng", SubPref: "off", Start: 195,
		}); err != nil {
			t.Fatal(err)
		}
		if asked() != 0 {
			t.Error("hevc не скопируется ни с какой точки — искать нечего")
		}
		if line := args(); !strings.Contains(line, "-ss 195.000") {
			t.Errorf("перекодирование обязано попадать в запрошенную секунду:\n%s", line)
		}
	})

	t.Run("HLS_ALLOW_COPY=0", func(t *testing.T) {
		m, _, asked := seekProbe(t, 186.186, true)
		m.AllowCopy = false
		if _, err := m.Start(StartOptions{
			Index: 0, Meta: copyableEpisode(), AudioPref: "eng", SubPref: "off", Start: 195,
		}); err != nil {
			t.Fatal(err)
		}
		if asked() != 0 {
			t.Error("рычаг выключает копирование целиком, вместе с поиском")
		}
	})
}

// TestSeekShiftsExternalSubtitlesByAlignedStart — метки внешних субтитров
// сдвигаются на ту же секунду, что ушла в -ss.
//
// Это и есть цена ошибки в выравнивании: сдвинь текст на запрошенные 195,
// пока видео начинается с 186.086, — и все реплики серии уедут на девять
// секунд. Заметить это на глаз можно только по фразам, а не по логам.
func TestSeekShiftsExternalSubtitlesByAlignedStart(t *testing.T) {
	m, _, _ := seekProbe(t, 186.186, true)

	srt := filepath.Join(t.TempDir(), "s03e23.srt")
	// Реплика на 190-й секунде: после выравнивания она обязана оказаться
	// на 190 - 186.086 = 3.914, а не за нулём, куда её увёл бы старт с 195.
	body := "1\n00:03:10,000 --> 00:03:12,000\nПривет\n"
	if err := os.WriteFile(srt, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := copyableEpisode()
	meta.Subtitles = []media.SubtitleTrack{
		{Index: -1, Code: "rus", Label: "Russian", Codec: "srt", SourcePath: srt},
	}

	snap, err := m.Start(StartOptions{
		Index: 0, Meta: meta, AudioPref: "eng", SubPref: "rus", Start: 195,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(m.TmpDir, "tms-hls-"+snap.ID)
	vtt, err := os.ReadFile(filepath.Join(dir, "subs0.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vtt), "00:03.914") {
		t.Errorf("реплика сдвинута не на выровненный start:\n%s", vtt)
	}
}
