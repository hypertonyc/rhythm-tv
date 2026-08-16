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

// TestRequestsGoThroughProxyPrefix — страница обязана строить адреса от пути
// текущего документа, а не от корня.
//
// Снаружи сервер публикуется reverse-proxy под путём с токеном (…/<токен>/…),
// и этот префикс прокси срезает. Запрос вида /api/status с такой страницы
// уходит МИМО префикса и получает 404 от самого nginx: сервер жив, страница
// открылась, а всё внутри неё мертво. Именно так это и выглядело на телефоне,
// пока адреса не стали собираться через apiUrl().
//
// Телевизора это не касается — у него полный адрес с токеном лежит
// в serverBase, — поэтому поломка живёт только здесь и только снаружи.
func TestRequestsGoThroughProxyPrefix(t *testing.T) {
	if !bytes.Contains(IndexHTML, []byte("function apiUrl(")) {
		t.Fatal("в странице нет apiUrl(): адреса собираются от корня")
	}

	// Дословные вызовы с корневым путём. Проверяются именно они, а не любое
	// вхождение «'/api/», потому что аргументом apiUrl() корневой путь
	// как раз и передаётся — это нормально.
	for _, bad := range []string{"xhrJson('/", "ping('/", "postJson('/", "video.src = '/"} {
		if bytes.Contains(IndexHTML, []byte(bad)) {
			t.Errorf("запрос мимо apiUrl(): %q — за прокси с токеном это 404", bad)
		}
	}

	// playlist приходит из ответа сервера корневым путём (/hls/<id>/index.m3u8),
	// и ему нужен тот же префикс, что и остальным запросам.
	if !bytes.Contains(IndexHTML, []byte("apiUrl(playlist)")) {
		t.Error("playlist из ответа сервера открывается без префикса прокси")
	}
}

// TestUploadInputHasNoAccept — фильтр по типу файла ломает загрузку с iPhone.
//
// iOS сопоставляет accept с UTI, а для .torrent и application/x-bittorrent
// их нет: Safari открывает «Файлы», где все файлы серые и выбрать нечего.
// Поле при этом выглядит рабочим, поэтому симптом — «не нажимается»,
// а не сообщение об ошибке. Фильтр здесь не нужен вовсе: метаинформацию
// разбирает сервер, и мусор он отвергает с 400 до записи на диск.
func TestUploadInputHasNoAccept(t *testing.T) {
	if bytes.Contains(IndexHTML, []byte("accept=")) {
		t.Error("на поле загрузки вернулся accept — с iPhone файл выбрать не получится")
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
