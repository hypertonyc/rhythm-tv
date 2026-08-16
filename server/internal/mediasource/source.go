// Package mediasource прячет за интерфейсом всё, что знает про торренты.
//
// Смысл границы двоякий. Во-первых, anacrolix импортируется ровно в одном файле
// (anacrolix.go), и заменить его можно не трогая HTTP-слой. Во-вторых, тесты
// поднимаются на Fake поверх обычных файлов — без роя, трекеров и ожидания пиров.
package mediasource

import (
	"context"
	"errors"
	"io"
)

// ErrNotReady возвращается, пока метаданные торрента не загружены.
var ErrNotReady = errors.New("torrent metadata is loading")

// File — то, что попадает в /api/files. Index это позиция в ПОЛНОМ списке файлов
// торрента, а не в отфильтрованном списке видео: телевизор хранит по нему
// rtv.positions и rtv.lastEpisode.
//
// Path — путь внутри торрента вместе с каталогами («Сезон 09/s09e01 - ….mkv»),
// и наружу он НЕ отдаётся: в /api/files уходит только Name, последний сегмент,
// как у webtorrent (ответ сверяется с Node-эталоном побайтово). Нужен он для
// порядка серий — номер сезона сплошь и рядом есть только в имени каталога.
type File struct {
	Index  int
	Name   string
	Path   string
	Length int64
}

// Stats — живые счётчики для /api/status.
type Stats struct {
	Peers         int
	DownloadSpeed float64
	Downloaded    int64
	Progress      float64
}

// Reader — чтение одного файла торрента.
//
// ReadContext, а не Read, потому что чтение может встать намертво на раздаче
// без пиров, и отменять его должен контекст HTTP-запроса. Close обязателен:
// пока Reader жив, вокруг его позиции держится окно приоритета, и торрент
// продолжает качаться даже после ухода клиента.
type Reader interface {
	io.Seeker
	io.Closer
	ReadContext(ctx context.Context, p []byte) (int, error)
	SetReadahead(int64)
}

// Source — один торрент.
//
// Активный торрент выбирается уровнем выше (internal/library): HTTP-слой
// спрашивает у библиотеки текущий Source перед каждой операцией. Сам интерфейс
// от этого не менялся и не должен — он описывает ровно один торрент.
//
// MULTI-TORRENT PIN: если когда-нибудь понадобится несколько ОДНОВРЕМЕННО
// активных торрентов, менять надо резолвер в httpapi (выбор источника по
// запросу) и hls.Manager (активный сеанс на торрент), а не этот интерфейс.
type Source interface {
	// Ready сообщает, загружены ли метаданные. Пока false, /api/files и
	// /api/probe обязаны отдавать 503, а /api/status — 200 с {"ready":false}.
	Ready() bool
	Name() string
	Files() []File
	Open(index int) (Reader, error)
	Stats() Stats
	Close() error
}
