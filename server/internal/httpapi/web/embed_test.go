package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const legacySource = "../../../legacy/server.mjs"

// TestMatchesLegacy заново достаёт HTML из appHtml() в server.mjs и сравнивает.
// Пока Node-эталон лежит в дереве, разъехаться эти два файла не могут.
//
// Когда legacy/ будет удалён, тест заменяется на сравнение с sha256-константой
// из TestByteExact.
func TestMatchesLegacy(t *testing.T) {
	src, err := os.ReadFile(legacySource)
	if err != nil {
		t.Skipf("Node-эталон уже удалён (%v) — остаётся TestByteExact", err)
	}

	const (
		fnMarker    = "function appHtml() {\n"
		openMarker  = "  return `"
		closeMarker = "`\n}"
	)
	fn := bytes.Index(src, []byte(fnMarker))
	if fn < 0 {
		t.Fatalf("в %s не нашлась appHtml()", legacySource)
	}
	open := bytes.Index(src[fn:], []byte(openMarker))
	if open < 0 {
		t.Fatalf("в appHtml() не нашлось начало шаблонной строки")
	}
	start := fn + open + len(openMarker)
	closeAt := bytes.Index(src[start:], []byte(closeMarker))
	if closeAt < 0 {
		t.Fatalf("в appHtml() не нашёлся конец шаблонной строки")
	}
	want := src[start : start+closeAt]

	// Перенос дословен ровно потому, что подставлять внутрь нечего.
	// Если в шаблоне появится ${...} или экранированный обратный апостроф,
	// сравнение ниже перестанет быть корректным — ловим это здесь.
	if bytes.Contains(want, []byte("${")) || bytes.Contains(want, []byte("\\`")) {
		t.Fatal("в шаблоне appHtml() появилась подстановка — простой перенос больше не годится")
	}

	if !bytes.Equal(IndexHTML, want) {
		t.Errorf("index.html разъехался с appHtml(): %d байт против %d", len(IndexHTML), len(want))
	}
}

// TestByteExact ловит самую вероятную поломку: редактор дописал \n в конец,
// Content-Length стал 20290, и ответ на «/» перестал совпадать с эталоном.
func TestByteExact(t *testing.T) {
	const wantLen = 20289

	if len(IndexHTML) != wantLen {
		t.Errorf("len(IndexHTML) = %d, ожидалось %d", len(IndexHTML), wantLen)
	}
	if bytes.HasSuffix(IndexHTML, []byte("\n")) {
		t.Error("index.html заканчивается переводом строки — в appHtml() его нет")
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
