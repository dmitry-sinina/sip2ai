// Package siplog provides an slog.Handler that uses tint for colorized output
// and handles multiline SIP protocol messages (containing \r\n) by printing
// them verbatim instead of quoting them onto a single line.
package siplog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/lmittmann/tint"
)

// LevelTrace is a custom slog level below Debug for high-frequency messages.
const LevelTrace = slog.Level(-8)

type Handler struct {
	tint  slog.Handler
	w     io.Writer
	mu    sync.Mutex
	attrs []slog.Attr
}

// New returns a Handler that uses tint for regular messages and prints
// multiline SIP protocol messages verbatim with real line breaks.
func New(w io.Writer, level slog.Level) *Handler {
	return &Handler{
		tint: tint.NewHandler(w, &tint.Options{
			Level:      level,
			TimeFormat: "2006-01-02 15:04:05.000",
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.LevelKey && a.Value.String() == "DEBUG-8" {
					a.Value = slog.StringValue("TRC")
				}
				return a
			},
		}),
		w: w,
	}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.tint.Enabled(ctx, l)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	// Prepend stored attrs (like cid) to the message as bare values,
	// then remove them from the record so tint doesn't render key=value.
	if len(h.attrs) > 0 {
		var prefix string
		for _, a := range h.attrs {
			prefix += a.Value.String() + " "
		}
		r.Message = prefix + r.Message
	}

	if !strings.ContainsAny(r.Message, "\r\n") {
		return h.tint.Handle(ctx, r)
	}
	// Multiline message: print with attrs on the first line, then raw body.
	ts := r.Time.Format("2006-01-02 15:04:05.000")
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.w, "%s %s", ts, levelString(r.Level))
	for _, a := range h.attrs {
		fmt.Fprintf(h.w, " %s", a.Value)
	}
	fmt.Fprintf(h.w, " %s\n", r.Message)
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Separate "cid" attrs (rendered as bare prefix in message) from
	// the rest (passed to tint for key=value rendering).
	var cidAttrs, tintAttrs []slog.Attr
	for _, a := range attrs {
		if a.Key == "cid" {
			cidAttrs = append(cidAttrs, a)
		} else {
			tintAttrs = append(tintAttrs, a)
		}
	}
	newH := &Handler{
		tint:  h.tint,
		w:     h.w,
		attrs: append(append([]slog.Attr(nil), h.attrs...), cidAttrs...),
	}
	if len(tintAttrs) > 0 {
		newH.tint = h.tint.WithAttrs(tintAttrs)
	}
	return newH
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{tint: h.tint.WithGroup(name), w: h.w}
}

func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	case l >= slog.LevelDebug:
		return "DBG"
	default:
		return "TRC"
	}
}
