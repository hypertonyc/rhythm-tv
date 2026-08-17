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
	"time"

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
		body = s.playerPlaylist(sessionID, raw)
	} else {
		f, err = os.Open(full)
		if err != nil {
			writeText(w, http.StatusNotFound, "Not ready", contentTypeText)
			return
		}
		defer f.Close()
		if strings.HasSuffix(fileName, ".ts") {
			// Плеер забрал сегмент — значит точку входа он уже выбрал,
			// и окно можно открывать целиком.
			s.notePlayerJoined(sessionID)
		}
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

// Окно входа: плеер начинает не там, где мы просим, а там, где велит правило
// HLS, — и единственный надёжный рычаг это то, ЧТО объявлено в плейлисте.
//
// Правило (RFC 8216, 6.3.3) прошивка исполняет буквально, и это измерено
// 17.08.2026 по логу nginx: в сеансе msx85f6j на втором запросе плейлиста было
// объявлено 17 сегментов, то есть 68.5 с при `TARGETDURATION:11`;
// 68.5 - 3*11 = 35.5, и последний сегмент, начинающийся не позже 35.5, —
// ровно `seg00008` с началом 33.7. Именно его телевизор и запросил первым.
//
// Отсюда способ: пока плеер не выбрал точку входа, объявлять не больше
// 3*TARGETDURATION. Тогда `конец - 3*TARGETDURATION <= 0`, и подходит только
// сегмент с началом 0. Хинты `EVENT` и `EXT-X-START` эта прошивка игнорирует
// (проверено выкаткой), а арифметику — нет.
const (
	// joinBudgetTargetDurations — тот самый множитель из правила. Меньше брать
	// незачем: чем шире окно, тем больше у плеера запас, прежде чем ему
	// понадобятся сегменты за окном.
	joinBudgetTargetDurations = 3

	// joinFuse — предохранитель. Если плеер так и не забрал ни одного сегмента
	// (например, эта прошивка на коротком плейлисте не входит, а ждёт роста),
	// окно всё равно открывается, и поведение вырождается в прежнее — с потерей
	// начала, но без вечного ожидания. Наблюдённое время до выбора — 2 с,
	// так что 20 с это запас в десять раз.
	joinFuse = 20 * time.Second
)

// notePlayerJoined запоминает, что плеер этого сеанса уже забирал сегменты.
//
// Состояние одно на весь сервер, а не на сеанс: подрезается только живой
// сеанс (у доигранного есть ENDLIST), а живой ровно один — новый /api/start
// гасит предыдущий. Так не нужен ни рост карты на каждую перемотку,
// ни её чистка.
func (s *Server) notePlayerJoined(sessionID string) {
	s.joinMu.Lock()
	s.joinedSession = sessionID
	s.joinMu.Unlock()
}

func (s *Server) playerJoined(sessionID string) bool {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	return s.joinedSession == sessionID
}

// playerPlaylist готовит плейлист к отдаче плееру.
func (s *Server) playerPlaylist(sessionID string, raw []byte) []byte {
	body := withStartAtBeginning(raw)

	// Доигранный плейлист не трогаем вовсе: с ENDLIST правило входа
	// не действует, плеер и так начинает с начала. Это же условие спасает
	// подобранный после выкатки сеанс — его смотрят с середины, и подрезать
	// у него список сегментов значило бы выбить из-под плеера то, что он играет.
	if bytes.Contains(raw, []byte("#EXT-X-ENDLIST")) {
		return body
	}
	if s.playerJoined(sessionID) {
		return body
	}
	snap, ok := s.deps.HLS.Get(sessionID)
	if !ok {
		return body
	}
	if time.Since(time.UnixMilli(snap.StartedAt)) > joinFuse {
		return body
	}
	return truncateToJoinWindow(body)
}

// truncateToJoinWindow оставляет в плейлисте начало не длиннее
// joinBudgetTargetDurations * TARGETDURATION.
//
// ENDLIST здесь появиться не может (выше проверено, что его нет), поэтому
// обрезанный хвост не выглядит концом потока: плеер перечитает плейлист
// и получит продолжение — к тому времени он уже забрал сегмент, и окно открыто.
//
// Плейлист, в котором не нашлось ни TARGETDURATION, ни сегментов, отдаётся
// как есть: портить непонятое хуже, чем отдать без правки.
func truncateToJoinWindow(playlist []byte) []byte {
	lines := bytes.SplitAfter(playlist, []byte("\n"))

	budget, ok := targetDuration(lines)
	if !ok {
		return playlist
	}
	budget *= joinBudgetTargetDurations

	var out []byte
	var spent float64
	for i := 0; i < len(lines); i++ {
		if !bytes.HasPrefix(lines[i], []byte("#EXTINF:")) {
			out = append(out, lines[i]...)
			continue
		}
		// Сегмент — это пара строк: #EXTINF и путь. Половинку отдать нельзя.
		if i+1 >= len(lines) {
			break
		}
		d, err := strconv.ParseFloat(string(bytes.TrimRight(
			bytes.TrimPrefix(lines[i], []byte("#EXTINF:")), ",\r\n")), 64)
		if err != nil {
			return playlist
		}
		// Первый сегмент оставляем всегда: он короче TARGETDURATION
		// по определению, но арифметику с плавающей точкой лучше
		// не заставлять это доказывать.
		if spent > 0 && spent+d > budget {
			break
		}
		spent += d
		out = append(out, lines[i]...)
		out = append(out, lines[i+1]...)
		i++
	}
	if spent == 0 {
		return playlist
	}
	return out
}

// targetDuration достаёт #EXT-X-TARGETDURATION — знаменатель правила входа.
func targetDuration(lines [][]byte) (float64, bool) {
	const tag = "#EXT-X-TARGETDURATION:"
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte(tag)) {
			continue
		}
		v, err := strconv.ParseFloat(string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte(tag)))), 64)
		if err != nil || v <= 0 {
			return 0, false
		}
		return v, true
	}
	return 0, false
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
