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
type File struct {
	Index  int
	Name   string
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
// MULTI-TORRENT PIN: мультиторрентность добавляется не сюда, а уровнем выше —
// заменой одного Source на реестр. Сам интерфейс при этом не меняется.
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
