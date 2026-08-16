package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const legacySource = "../../../legacy/server.mjs"

// TestDivergedFromLegacyOnPurpose.
//
// До появления библиотеки торрентов эта страница была ПОБАЙТОВОЙ копией
// appHtml() из server.mjs, и тест сравнивал их напрямую. Сравнивать больше
// нечего: в страницу добавлены загрузка .torrent с телефона и выбор активного
// торрента, а в Node-эталоне этого нет и не появится.
//
// Поэтому проверяется обратное утверждение — что расхождение осознанное,
// а не «страницу случайно откатили к эталону»: общий каркас (шапка, плеер)
// на месте, а новая часть не потерялась. Заодно это ловит правку, которая
// вырезала бы библиотеку целиком.
func TestDivergedFromLegacyOnPurpose(t *testing.T) {
	if _, err := os.ReadFile(legacySource); err != nil {
		t.Skipf("Node-эталон уже удалён (%v) — остаются структурные проверки", err)
	}

	// Куски, унаследованные от эталона: если их не стало, страницу
	// переписали целиком и сверять её с чем-либо больше нельзя.
	for _, marker := range []string{
		`<video id="video" controls playsinline></video>`,
		`'/api/start/'`,
		`'/api/hls-status/'`,
	} {
		if !bytes.Contains(IndexHTML, []byte(marker)) {
			t.Errorf("из страницы исчезло %q — это часть общего с эталоном плеера", marker)
		}
	}

	// Новая часть, которой в эталоне нет.
	for _, marker := range []string{`'/api/torrents'`, `id="torrentList"`} {
		if !bytes.Contains(IndexHTML, []byte(marker)) {
			t.Errorf("из страницы исчезла библиотека торрентов: нет %q", marker)
		}
	}
}

// TestByteExact ловит самую вероятную поломку: редактор дописал \n в конец,
// Content-Length разъехался с телом, и браузер получил обрезанную страницу.
//
// Точная длина здесь больше не проверяется: она имела смысл, пока страница
// была заморожена по эталону, а теперь менялась бы при каждой правке верстки.
func TestByteExact(t *testing.T) {
	if bytes.HasSuffix(IndexHTML, []byte("\n")) {
		t.Error("index.html заканчивается переводом строки — редактор дописал его молча")
	}
	if !bytes.HasSuffix(IndexHTML, []byte("</html>")) {
		t.Error("index.html не заканчивается на </html>")
	}
	if !bytes.HasPrefix(IndexHTML, []byte("<!doctype html>\n")) {
		t.Error("index.html не начинается с <!doctype html>")
	}
	// Страница обязана оставаться самодостаточной: она ходит только
	// по относительным путям того же origin, иначе токен в пути от прокси
	// перестанет подставляться.
	if strings.Contains(string(IndexHTML), "http://") || strings.Contains(string(IndexHTML), "https://") {
		t.Error("в index.html появился абсолютный URL — прокси с токеном в пути его не перепишет")
	}
}
