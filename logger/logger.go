package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorDebug  = "\033[36m" // cyan
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

// New creates a pretty colored slog.Logger for terminal output.
// DEBUG=cyan, INFO=green, WARN=yellow, ERROR=red
func New() *slog.Logger {
	return slog.New(&colorHandler{
		writer: os.Stdout,
		level:  slog.LevelDebug,
	})
}

type colorHandler struct {
	mu     sync.Mutex
	writer io.Writer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

func (h *colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *colorHandler) Handle(_ context.Context, r slog.Record) error {
	// Build colored line manually — no escaping
	var buf strings.Builder

	// Timestamp
	buf.WriteString(time.Now().Format("15:04:05.000"))
	buf.WriteByte(' ')

	// Level (colored)
	levelStr := r.Level.String()
	color := colorFor(r.Level)
	buf.WriteString(color)
	buf.WriteString(levelStr)
	buf.WriteString(colorReset)
	buf.WriteByte(' ')

	// Source (file:line)
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		if f.File != "" {
			src := filepath.Base(f.File)
			buf.WriteString(colorGray)
			buf.WriteString(fmt.Sprintf("%s:%d", src, f.Line))
			buf.WriteString(colorReset)
			buf.WriteByte(' ')
		}
	}

	// Groups prefix
	for _, g := range h.groups {
		buf.WriteString(g)
		buf.WriteByte('.')
	}

	// Message (colored)
	buf.WriteString(color)
	buf.WriteString(r.Message)
	buf.WriteString(colorReset)

	// Attrs from handler + record
	attrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	for _, a := range attrs {
		if a.Key == "" {
			continue
		}
		buf.WriteByte(' ')
		buf.WriteString(colorGray)
		buf.WriteString(a.Key)
		buf.WriteString(colorReset)
		buf.WriteByte('=')
		buf.WriteString(fmt.Sprint(a.Value.Any()))
	}

	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.writer, buf.String())
	return err
}

func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newH := h.clone()
	newH.attrs = append(newH.attrs, attrs...)
	return newH
}

func (h *colorHandler) WithGroup(name string) slog.Handler {
	newH := h.clone()
	newH.groups = append(newH.groups, name)
	return newH
}

func (h *colorHandler) clone() *colorHandler {
	attrs := make([]slog.Attr, len(h.attrs))
	copy(attrs, h.attrs)
	groups := make([]string, len(h.groups))
	copy(groups, h.groups)
	return &colorHandler{
		writer: h.writer,
		level:  h.level,
		attrs:  attrs,
		groups: groups,
	}
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
		return colorDebug
	}
}