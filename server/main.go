// Команда server — медиасервер Rhythm TV: раздаёт файлы из торрента
// и перекодирует их в HLS для телевизоров Samsung на Tizen 2.3.
//
// Один процесс — один торрент, один активный сеанс перекодирования.
// Путь к .torrent — обязательный аргумент командной строки.
//
//	server /data/file.torrent
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/avdav/torrent-media/server/internal/hls"
	"github.com/avdav/torrent-media/server/internal/httpapi"
	"github.com/avdav/torrent-media/server/internal/media"
	"github.com/avdav/torrent-media/server/internal/mediasource"
)

func main() {
	port := envInt("PORT", 8000)

	// Проверка живости для HEALTHCHECK в образе. Отдельный флаг, а не curl,
	// потому что рантайм-образ иначе пришлось бы раздувать ради одного запроса.
	// Бьём в /api/status: он отдаёт 200 ещё до загрузки метаданных, тогда как
	// /api/files в это время отвечает 503 и контейнер флапал бы на каждом старте.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(port))
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: server /data/file.torrent")
		os.Exit(1)
	}
	torrentPath := os.Args[1]
	// Таймаут выбран чуть меньше 30-секундного XHR-таймаута телевизора,
	// чтобы клиент увидел внятную ошибку, а не оборванный запрос.
	probeTimeout := time.Duration(envInt("PROBE_TIMEOUT_MS", 25000)) * time.Millisecond
	// Любое значение кроме "0" включает копирование дорожек без перекодирования.
	allowCopy := os.Getenv("HLS_ALLOW_COPY") != "0"
	store := os.Getenv("TORRENT_STORE")
	if store == "" {
		store = filepath.Join(os.TempDir(), "webtorrent")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source, err := mediasource.NewAnacrolix(mediasource.Options{
		TorrentPath: torrentPath,
		DataDir:     store,
		ListenPort:  envInt("TORRENT_PORT", 0),
		Seed:        os.Getenv("TORRENT_SEED") == "1",
		// На проде включено: иначе каждый деплой стирает уже скачанное.
		PersistStore: os.Getenv("TORRENT_STORE_PERSIST") == "1",
		// Нужно один раз при переезде с Node: раскладка на диске совпадает,
		// а база готовности кусков у anacrolix своя и пустая.
		VerifyOnStart: os.Getenv("TORRENT_VERIFY_ON_START") == "1",
	})
	if err != nil {
		log.Fatalf("torrent: %v", err)
	}

	// ffmpeg и ffprobe читают торрент петлёй через наш же HTTP-порт.
	// Это не обходной путь, а несущая конструкция: на каждой перемотке ffmpeg
	// рвёт ответ и заходит новым GET с новым Range, и именно так перекодирование
	// из торрента вообще становится возможным.
	rawURL := func(index int) string {
		return fmt.Sprintf("http://127.0.0.1:%d/raw/%d", port, index)
	}

	prober := &media.Prober{RawURL: rawURL, Timeout: probeTimeout}
	manager := &hls.Manager{
		AllowCopy:  allowCopy,
		RawURL:     rawURL,
		Downloaded: func() int64 { return source.Stats().Downloaded },
	}

	// Подбираем каталоги сеансов, оставшиеся от прежнего процесса: без этого
	// выкатка обрывала бы просмотр — у нового процесса нет состояния сеансов,
	// и на /hls/<id>/... он отвечал бы 404. Делается ДО начала обслуживания.
	if n := manager.AdoptOrphans(); n > 0 {
		log.Printf("adopted %d session(s) from previous run", n)
	}

	handler := httpapi.New(httpapi.Deps{
		Source:  source,
		Prober:  prober,
		HLS:     manager,
		BaseCtx: ctx,
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", port),
		Handler: handler,
		// WriteTimeout НЕ ставится сознательно: ответ /raw, которым питается
		// ffmpeg, на медленной раздаче законно длится часами.
		ReadHeaderTimeout: 20 * time.Second,
	}

	go func() {
		log.Printf("HTTP server listening on :%d", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	shutdown(srv, manager, source, cancel)
}

var shutdownOnce sync.Once

func shutdown(srv *http.Server, manager *hls.Manager, source mediasource.Source, cancel context.CancelFunc) {
	shutdownOnce.Do(func() {
		// Сторожевой таймер несущий, а не перестраховка: srv.Shutdown не может
		// прервать горутину, стоящую внутри чтения из торрента, и без него
		// процесс завис бы на раздаче без пиров.
		time.AfterFunc(5*time.Second, func() {
			log.Println("shutdown watchdog fired")
			os.Exit(0)
		})

		manager.Shutdown()
		cancel()

		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(ctx)
		_ = source.Close()
		os.Exit(0)
	})
}

// healthcheck возвращает код возврата для docker HEALTHCHECK.
func healthcheck(port int) int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("%s=%q is not a number, using %d", name, raw, fallback)
		return fallback
	}
	return n
}
