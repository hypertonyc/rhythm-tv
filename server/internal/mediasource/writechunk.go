package mediasource

import (
	"log"
	"sync"
	"time"
)

// writeChunkQuiet — окно тишины в логе: обработчик зовётся на КАЖДЫЙ неудачный
// кусок, и на полном диске это десятки строк в секунду.
const writeChunkQuiet = 30 * time.Second

// onWriteChunkError отдаёт обработчик ошибки записи куска на диск, который
// только пишет в лог и НЕ выключает скачивание.
//
// Умолчание anacrolix (Torrent.onWriteChunkErr) на любую такую ошибку зовёт
// disallowDataDownloadLocked, и это состояние необратимо: AllowDataDownload
// сам он не вызывает никогда, а у нас его нет. Цена умолчания измерена
// 02.09.2026: диск на VPS заполнился, запись куска вернула ENOSPC — и сервер
// перестал качать НАСОВСЕМ. Дальше это выглядело так: любое чтение падало
// мгновенно с «torrent data downloading disabled», в рой клиент не шёл
// (peers 0), ffmpeg получал Input/output error на /raw и выходил с кодом 1,
// а на телевизоре был чёрный экран — до перезапуска процесса руками.
//
// Нам такое поведение не годится, потому что кончившееся место у нас
// не авария, а рабочий режим: сервер держит хранилище впритык и сам чистит
// его раз в минуту (internal/reclaim). ENOSPC здесь — состояние на минуту,
// после которой недокачанные куски будут запрошены заново. Единственное,
// что нужно от обработчика, — чтобы он не превращал эту минуту в вечность
// и оставил след в логе: ENOSPC значит, что чистка не справляется, и это
// стоит увидеть до того, как диск встанет.
func onWriteChunkError(name string) func(error) {
	var (
		mu         sync.Mutex
		lastAt     time.Time
		suppressed int
	)
	return func(err error) {
		mu.Lock()
		defer mu.Unlock()

		now := time.Now()
		if !lastAt.IsZero() && now.Sub(lastAt) < writeChunkQuiet {
			suppressed++
			return
		}
		if suppressed > 0 {
			log.Printf("торрент %q: запись куска не удалась: %v (и ещё %d таких за %s; скачивание оставлено включённым)",
				name, err, suppressed, writeChunkQuiet)
		} else {
			log.Printf("торрент %q: запись куска не удалась: %v (скачивание оставлено включённым)", name, err)
		}
		lastAt, suppressed = now, 0
	}
}
