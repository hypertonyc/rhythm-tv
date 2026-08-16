package mediasource

import (
	"context"
	"log/slog"
	"os"
	"strings"

	alog "github.com/anacrolix/log"
)

// Логи anacrolix глушатся по одной причине: он пишет ERR на каждое отменённое
// чтение, а отменяем его мы сами. ffmpeg рвёт HTTP-ответ и заходит новым Range
// на КАЖДОЙ перемотке (телевизор перематывает перезапуском HLS), и каждый такой
// обрыв давал три строки ERR из reader.go. В проде с ротацией по 10 МБ этот шум
// вытеснил бы настоящие ошибки, ради которых лог и читают.
//
// Фильтров два, потому что в библиотеке два пути логирования: свой alog
// и стандартный slog. Сообщения ридера идут именно через slog (reader.go
// зовёт t.slogger()), так что без второго фильтра первый бесполезен.

// quietLogger — аналоговый логгер anacrolix (портфорвардинг, трекеры, пиры).
func quietLogger() alog.Logger {
	l := alog.Default
	l.Handlers = []alog.Handler{cancelFilter{next: alog.DefaultHandler}}
	return l
}

type cancelFilter struct{ next alog.Handler }

func (f cancelFilter) Handle(r alog.Record) {
	if strings.Contains(r.Text(), "context canceled") {
		return
	}
	f.next.Handle(r)
}

// quietSlogger — тот логгер, которым пишет reader.go.
func quietSlogger() *slog.Logger {
	return slog.New(slogCancelFilter{
		next: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
	})
}

type slogCancelFilter struct{ next slog.Handler }

func (f slogCancelFilter) Enabled(ctx context.Context, level slog.Level) bool {
	return f.next.Enabled(ctx, level)
}

func (f slogCancelFilter) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "context canceled") {
		return nil
	}
	drop := false
	r.Attrs(func(a slog.Attr) bool {
		// Ключ "err" — то, как anacrolix кладёт причину в reader.go.
		if a.Key == "err" && strings.Contains(a.Value.String(), "context canceled") {
			drop = true
			return false
		}
		return true
	})
	if drop {
		return nil
	}
	return f.next.Handle(ctx, r)
}

func (f slogCancelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return slogCancelFilter{next: f.next.WithAttrs(attrs)}
}

func (f slogCancelFilter) WithGroup(name string) slog.Handler {
	return slogCancelFilter{next: f.next.WithGroup(name)}
}
