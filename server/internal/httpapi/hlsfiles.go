package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/avdav/torrent-media/server/internal/hls"
)

// hlsFileName — /^[A-Za-z0-9._-]+\.(m3u8|ts|vtt)$/ БЕЗ флага /i.
//
// Регистр здесь важен и расходится с маршрутом намеренно: маршрут в роутере
// регистронезависим, поэтому «index.M3U8» до обработчика доходит, а вот эта
// проверка его отвергает с кодом 400. Асимметрия унаследована от оригинала.
var hlsFileName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(m3u8|ts|vtt)$`)

// serveHLSFile отдаёт файл из каталога сеанса.
//
// Range здесь НЕ поддерживается сознательно: оригинал отвечал только 200
// и целым файлом. Поэтому http.ServeContent/ServeFile не годятся — они добавят
// Accept-Ranges и ETag и начнут честно отдавать 206, а AVPlay на телевизоре
// вполне способен прислать Range на .ts, и его поведение изменится.
func (s *Server) serveHLSFile(w http.ResponseWriter, r *http.Request, sessionID, fileName string) {
	dir, ok := s.deps.HLS.SessionDir(sessionID)
	if !ok {
		writeText(w, http.StatusNotFound, "HLS session not found", contentTypeText)
		return
	}
	if !hlsFileName.MatchString(fileName) {
		writeText(w, http.StatusBadRequest, "Bad HLS path", contentTypeText)
		return
	}

	// Тип выбирается по СТРОЧНОМУ суффиксу, как endsWith в оригинале.
	contentType := "video/mp2t"
	cacheControl := "public, max-age=3600"
	switch {
	case strings.HasSuffix(fileName, ".m3u8"):
		contentType = "application/vnd.apple.mpegurl"
		// Плейлист дописывается по мере появления сегментов, кэшировать нельзя.
		cacheControl = "no-store"
	case strings.HasSuffix(fileName, ".vtt"):
		contentType = "text/vtt; charset=utf-8"
	}

	full := filepath.Join(dir, fileName)
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
		return
	}

	// Плейлист плеера — десяток килобайт, и отдаётся он не как есть, поэтому
	// читается в память целиком. Сегменты по-прежнему стримятся: .ts бывает
	// на несколько мегабайт.
	//
	// Побочно это убирает гонку с ffmpeg: он переписывает плейлист через
	// временный файл и rename, и между Stat и Open мог подсунуться уже другой
	// файл — Content-Length от одного, тело от другого.
	var body []byte
	var f *os.File
	if fileName == hls.PlaylistName {
		raw, readErr := os.ReadFile(full)
		if readErr != nil {
			writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
			return
		}
		body = withStartAtBeginning(raw)
	} else {
		f, err = os.Open(full)
		if err != nil {
			writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
			return
		}
		defer f.Close()
	}

	size := st.Size()
	if body != nil {
		size = int64(len(body))
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Cache-Control", cacheControl)
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if body != nil {
		_, _ = w.Write(body)
		return
	}
	_, _ = io.Copy(w, f)
}

// startTag — «предпочтительная точка старта: начало».
//
// Второй хинт к #EXT-X-PLAYLIST-TYPE:EVENT из hls.BuildArgs, и оба стоят вместе
// намеренно. Правило «не входить ближе трёх TARGETDURATION к концу»
// (RFC 8216, 6.3.3) привязано к отсутствию EXT-X-ENDLIST, а не к типу
// плейлиста, так что прошивка вправе применять его и к EVENT. EXT-X-START
// отвечает на тот же вопрос прямо (RFC 8216, 4.3.5.2), но он новее, и понимает
// ли его AVPlay 2015 года — неизвестно. Какой из двух сработает, покажет лог
// nginx: телевизор обязан начинать с seg00000.
//
// Дописывается на отдаче, потому что у hlsenc такой опции нет вовсе
// (проверено на ffmpeg 5.1 из прод-образа). Только в index.m3u8:
// index_vtt.m3u8 разбирает наш же код, а master.m3u8 не читает никто.
const startTag = "#EXT-X-START:TIME-OFFSET=0"

// withStartAtBeginning вставляет startTag сразу после #EXTM3U — тег
// плейлистного уровня, и место до первого #EXTINF для него законное.
//
// Плейлист без #EXTM3U не трогается: такого от ffmpeg не бывает, а портить
// то, чего не понял, хуже, чем отдать как есть. Версия протокола не поднимается
// сознательно: незнакомый тег плеер обязан пропустить, а вот незнакомую версию
// он вправе отвергнуть целиком.
func withStartAtBeginning(playlist []byte) []byte {
	if bytes.Contains(playlist, []byte(startTag)) {
		return playlist
	}
	const header = "#EXTM3U\n"
	if !bytes.HasPrefix(playlist, []byte(header)) {
		return playlist
	}
	out := make([]byte, 0, len(playlist)+len(startTag)+1)
	out = append(out, header...)
	out = append(out, startTag...)
	out = append(out, '\n')
	return append(out, playlist[len(header):]...)
}
