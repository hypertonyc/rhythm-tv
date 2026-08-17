package web

import (
	"bytes"
	"strings"
	"testing"
)

// TestPageIsControlOnly — страница управляет сервером и НЕ показывает видео.
//
// Раньше здесь жил тот же плеер, что и на телевизоре: страница была побайтовой
// копией appHtml() из server.mjs, и тест сравнивал их напрямую. Потом
// в неё добавили библиотеку торрентов, и сравнение сменилось на «каркас
// эталона на месте, новая часть не потерялась». Теперь ушёл и каркас:
// с телефона сервером управляют, а смотрят с телевизора, и <video> здесь
// означал бы вторую копию логики плеера, которую никто не открывает.
//
// Проверяется именно ОТСУТСТВИЕ плеера, потому что вернуть его случайно легко:
// половина обработчиков (/api/start, /api/probe, /api/hls-status) на месте
// и работает — их зовёт телевизор.
func TestPageIsControlOnly(t *testing.T) {
	for _, gone := range []string{"<video", "/api/start/", "/api/hls-status/", "/api/probe/"} {
		if bytes.Contains(IndexHTML, []byte(gone)) {
			t.Errorf("на страницу вернулся плеер: найдено %q", gone)
		}
	}
}

// TestThreeScreensAreThere — экранов ровно три, и каждый со своим источником.
//
// Тест грубый и намеренно такой: он ловит не вёрстку, а пропажу целого экрана
// вместе с его ручкой — например, правку, которая вырезала бы дашборд
// и оставила мёртвую вкладку.
func TestThreeScreensAreThere(t *testing.T) {
	for _, marker := range []string{
		// Торренты: библиотека и место под скачанное.
		`id="screen-torrents"`, `id="torrentList"`, `'/api/torrents'`,
		// Сеанс: снимок воспроизведения и остановка.
		`id="screen-session"`, `'/api/status'`, `'/api/stop'`,
		// Дашборд: показания машины и внутренние счётчики.
		`id="screen-metrics"`, `'/api/metrics'`,
	} {
		if !bytes.Contains(IndexHTML, []byte(marker)) {
			t.Errorf("со страницы исчезло %q", marker)
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
	if !bytes.Contains(IndexHTML, []byte("fetch(apiUrl(path)")) {
		t.Error("единственная обёртка над fetch перестала подставлять префикс")
	}

	// Дословные вызовы с корневым путём. Проверяется именно вызов сети,
	// а не любое вхождение «'/api/»: аргументом apiUrl() корневой путь
	// как раз и передаётся — это нормально.
	for _, bad := range []string{`fetch('/`, `fetch("/`, "fetch(`/", `new XMLHttpRequest`} {
		if bytes.Contains(IndexHTML, []byte(bad)) {
			t.Errorf("запрос мимо apiUrl(): %q — за прокси с токеном это 404", bad)
		}
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
// Точная длина здесь не проверяется: она имела смысл, пока страница была
// заморожена по эталону, а теперь менялась бы при каждой правке вёрстки.
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
	// перестанет подставляться. Заодно это запрещает внешние шрифты и графики
	// с CDN — за прокси в домашней сети они просто не загрузятся.
	//
	// Отсюда же следует, что графики рисуются на <canvas>, а не в SVG:
	// у inline-SVG обязателен xmlns, а это абсолютный адрес.
	if strings.Contains(string(IndexHTML), "http://") || strings.Contains(string(IndexHTML), "https://") {
		t.Error("в index.html появился абсолютный URL — прокси с токеном в пути его не перепишет")
	}
}
