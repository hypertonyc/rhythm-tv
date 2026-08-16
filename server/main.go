// Команда server — медиасервер Rhythm TV: раздаёт файлы из торрента
// и перекодирует их в HLS для телевизоров Samsung на Tizen 2.3.
//
// Торрентов на сервере может лежать сколько угодно (каталог TORRENT_LIB,
// пополняется загрузкой с телефона), но АКТИВЕН всегда ровно один: его серии
// видит телевизор, и на него же работает единственный сеанс перекодирования.
//
//	server /data/file.torrent   # каталог библиотеки = каталог файла,
//	                            # сам файл включается при первом запуске
//	TORRENT_LIB=/data server    # без аргумента: активный берётся из .tms-active
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
	"github.com/avdav/torrent-media/server/internal/library"
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

	// Аргумент необязателен: если он есть, торрент заносится в библиотеку
	// и включается при первом запуске, а дальше выбор живёт в .tms-active
	// и меняется с телефона. Каталог библиотеки — TORRENT_LIB, а без него
	// каталог самого файла: так старая команда запуска работает как раньше.
	torrentPath := ""
	if len(os.Args) > 1 {
		torrentPath = os.Args[1]
	}
	libDir := os.Getenv("TORRENT_LIB")
	if libDir == "" {
		if torrentPath == "" {
			fmt.Fprintln(os.Stderr, "Usage: server /data/file.torrent   (или TORRENT_LIB=/data server)")
			os.Exit(1)
		}
		libDir = filepath.Dir(torrentPath)
	}

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

	client, err := mediasource.NewClient(mediasource.Options{
		DataDir:    store,
		ListenPort: envInt("TORRENT_PORT", 0),
		Seed:       os.Getenv("TORRENT_SEED") == "1",
		// На проде включено: иначе каждый деплой стирает уже скачанное.
		PersistStore: os.Getenv("TORRENT_STORE_PERSIST") == "1",
		// Нужно один раз при переезде с Node: раскладка на диске совпадает,
		// а база готовности кусков у anacrolix своя и пустая.
		VerifyOnStart: os.Getenv("TORRENT_VERIFY_ON_START") == "1",
		// Разогрев роя: без него первый сеанс после перезапуска попадает
		// на пустой рой и умирает. 256 КБ хватает, чтобы клиент объявился
		// трекерам и поднял DHT.
		WarmupBytes: int64(envInt("TORRENT_WARMUP_KB", 256)) << 10,
	})
	if err != nil {
		log.Fatalf("torrent: %v", err)
	}

	lib := library.New(libDir, store, func(path string) (mediasource.Source, error) {
		// Явное присваивание интерфейсу, а не return client.Add(path):
		// иначе при ошибке наружу уехал бы типизированный nil, который
		// сравнение с nil не проходит.
		t, err := client.Add(path)
		if err != nil {
			return nil, err
		}
		return t, nil
	})

	// Отсутствие активного торрента — не повод не стартовать: сервер поднимется
	// с пустой библиотекой, отдавая ready:false, и первый же загруженный
	// с телефона .torrent сам станет активным.
	switch entry, err := lib.Restore(torrentPath); {
	case err != nil:
		log.Printf("библиотека %s: %v", libDir, err)
	case entry.ID == "":
		log.Printf("библиотека %s: активного торрента нет, ждём загрузки", libDir)
	default:
		log.Printf("библиотека %s: активен %q (%s)", libDir, entry.Name, entry.ID)
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
		AllowCopy: allowCopy,
		RawURL:    rawURL,
		// Счётчик берётся у активного торрента на каждый вызов: после
		// переключения downloadedSinceStart должен считаться по новому.
		Downloaded: func() int64 {
			src := lib.Current()
			if src == nil {
				return 0
			}
			return src.Stats().Downloaded
		},
	}

	// Подбираем каталоги сеансов, оставшиеся от прежнего процесса: без этого
	// выкатка обрывала бы просмотр — у нового процесса нет состояния сеансов,
	// и на /hls/<id>/... он отвечал бы 404. Делается ДО начала обслуживания.
	if n := manager.AdoptOrphans(); n > 0 {
		log.Printf("adopted %d session(s) from previous run", n)
	}

	handler := httpapi.New(httpapi.Deps{
		Library: lib,
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
	shutdown(srv, manager, lib, client, cancel)
}

var shutdownOnce sync.Once

func shutdown(srv *http.Server, manager *hls.Manager, lib *library.Library, client *mediasource.Client, cancel context.CancelFunc) {
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
		// Сначала активный торрент, потом клиент: Client.Close по умолчанию
		// сносит хранилище целиком, и делать это до снятия торрента незачем.
		_ = lib.Close()
		_ = client.Close()
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
