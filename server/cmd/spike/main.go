// Команда spike — разведка перед портом: проверяет, что anacrolix/torrent
// видит торрент так же, как webtorrent.
//
// Это go/no-go всего порта. Телевизор хранит rtv.positions и rtv.lastEpisode
// по ИНДЕКСУ файла в торренте, поэтому расхождение в порядке или в имени файла
// означает, что после переезда все сохранённые позиции укажут на чужие серии.
//
// Печатает ровно тот JSON, который отдаёт /api/files, чтобы его можно было
// побайтово сравнить с ответом Node-эталона.
//
// Использование:
//
//	go run ./cmd/spike -torrent data/tbbt.torrent            # JSON /api/files
//	go run ./cmd/spike -torrent data/tbbt.torrent -all       # + невидеофайлы
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/avdav/torrent-media/server/internal/jscompat"
	"github.com/avdav/torrent-media/server/internal/media"
)

type fileEntry struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

type filesResponse struct {
	Torrent string      `json:"torrent"`
	Files   []fileEntry `json:"files"`
}

func main() {
	torrentPath := flag.String("torrent", "", "путь к .torrent")
	showAll := flag.Bool("all", false, "показать все файлы, а не только видео")
	readIndex := flag.Int("read", -1, "прочитать первые -bytes байт файла с этим индексом (нужна сеть)")
	readBytes := flag.Int64("bytes", 1<<20, "сколько байт читать при -read")
	flag.Parse()

	if *torrentPath == "" {
		fmt.Fprintln(os.Stderr, "usage: spike -torrent FILE.torrent [-read N]")
		os.Exit(1)
	}

	offline := *readIndex < 0

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = mustTempDir()
	// Без -read сеть не нужна вовсе: метаданные лежат в самом .torrent.
	cfg.DisableTCP, cfg.DisableUTP = offline, offline
	cfg.NoDHT = offline
	cfg.DisableTrackers = offline
	cfg.Debug = false

	client, err := torrent.NewClient(cfg)
	if err != nil {
		fatal("torrent.NewClient: %v", err)
	}
	defer client.Close()

	t, err := client.AddTorrentFromFile(*torrentPath)
	if err != nil {
		fatal("AddTorrentFromFile: %v", err)
	}
	// Метаданные лежат в самом .torrent, так что ждать нечего — но канал
	// всё равно надо дождаться, иначе Files() пуст.
	<-t.GotInfo()

	// DownloadAll() НЕ вызывается: в anacrolix куски по умолчанию имеют
	// приоритет None, и это ровно то, что в webtorrent даёт deselect:true.
	// Данные потянутся только при создании Reader'а на конкретный файл.

	all := t.Files()
	files := make([]fileEntry, 0, len(all))
	for i, f := range all {
		name := path.Base(f.DisplayPath())
		if !*showAll && !media.IsVideoName(name) {
			continue
		}
		files = append(files, fileEntry{Index: i, Name: name, Length: f.Length()})
	}

	out, err := jscompat.Marshal(filesResponse{Torrent: t.Name(), Files: files})
	if err != nil {
		fatal("marshal: %v", err)
	}
	fmt.Println(string(out))

	fmt.Fprintf(os.Stderr, "\n# всего файлов: %d, из них видео: %d\n", len(all), len(files))
	fmt.Fprintf(os.Stderr, "# имя торрента: %q\n", t.Name())
	if len(all) > 0 {
		fmt.Fprintf(os.Stderr, "# первый DisplayPath: %q\n", all[0].DisplayPath())
		fmt.Fprintf(os.Stderr, "# последний DisplayPath: %q\n", all[len(all)-1].DisplayPath())
	}

	if *readIndex >= 0 {
		readCheck(t, all, *readIndex, *readBytes)
	}
}

// readCheck проверяет вторую половину гипотезы: до создания Reader'а не качается
// ничего, а Reader тянет данные с нужного смещения. Это и есть аналог
// webtorrent-овского createReadStream поверх deselect:true.
func readCheck(t *torrent.Torrent, all []*torrent.File, index int, want int64) {
	if index >= len(all) {
		fatal("индекс %d вне списка из %d файлов", index, len(all))
	}
	f := all[index]

	fmt.Fprintf(os.Stderr, "\n# до чтения: BytesCompleted=%d (ожидается 0 — приоритет кусков None)\n",
		t.BytesCompleted())

	r := f.NewReader()
	defer r.Close() // без этого окно приоритета живёт вечно и торрент качается дальше
	r.SetReadahead(8 << 20)

	buf := make([]byte, 64<<10)
	var got int64
	start := time.Now()
	for got < want {
		n, err := r.Read(buf)
		got += int64(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "# чтение прервано на %d байт: %v\n", got, err)
			break
		}
		if time.Since(start) > 90*time.Second {
			fmt.Fprintf(os.Stderr, "# таймаут на %d байт\n", got)
			break
		}
	}

	fmt.Fprintf(os.Stderr, "# прочитано %d байт за %s\n", got, time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "# после чтения: BytesCompleted=%d, пиров %d\n",
		t.BytesCompleted(), t.Stats().ActivePeers)

	// Тот же файл с середины: ffmpeg на каждой перемотке делает ровно это —
	// рвёт ответ и заходит новым Range с другого смещения.
	mid := f.Length() / 2
	r2 := f.NewReader()
	defer r2.Close()
	if _, err := r2.Seek(mid, io.SeekStart); err != nil {
		fmt.Fprintf(os.Stderr, "# Seek(%d) не удался: %v\n", mid, err)
		return
	}
	n, err := io.ReadFull(r2, buf[:4096])
	fmt.Fprintf(os.Stderr, "# чтение с середины (offset %d): %d байт, err=%v\n", mid, n, err)
}

func mustTempDir() string {
	dir, err := os.MkdirTemp("", "tms-spike-")
	if err != nil {
		fatal("MkdirTemp: %v", err)
	}
	return dir
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
