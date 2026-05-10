package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

// New creates a pretty colored slog.Logger for terminal output.
// DEBUG=gray, INFO=green, WARN=yellow, ERROR=red
func New() *slog.Logger {
	replace := func(groups []string, a slog.Attr) slog.Attr {
		// hide default time key — color prefix replaces it visually
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}

	handler := &colorHandler{
		inner: slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: replace,
		}),
	}

	return slog.New(handler)
}

// colorHandler wraps slog.TextHandler and injects ANSI color per log level.
type colorHandler struct {
	inner slog.Handler
}

func (h *colorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *colorHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = fmt.Sprintf("%s%s%s", colorFor(r.Level), r.Message, colorReset)
	return h.inner.Handle(ctx, r)
}

func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &colorHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *colorHandler) WithGroup(name string) slog.Handler {
	return &colorHandler{inner: h.inner.WithGroup(name)}
}

func colorFor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return colorRed
	case level >= slog.LevelWarn:
		return colorYellow
	case level >= slog.LevelInfo:
		return colorGreen
	default:
		return colorGray
	}
}
