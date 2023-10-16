package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

type consoleHandler struct {
	w      io.Writer
	l      slog.Leveler
	attrs  []slog.Attr
	prefix string
}

func newConsoleHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &consoleHandler{
		w: w,
		l: level,
	}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.l.Level() <= level
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var attrs []string
	r.AddAttrs(h.attrs...)
	r.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr.String())
		return true
	})
	// Max length we anticipate for level: DEBUG+2
	_, err := fmt.Fprintf(h.w, "%s [%-7s] %s\t%s\n",
		r.Time.Format(time.RFC3339),
		r.Level.String(),
		r.Message,
		strings.Join(attrs, "\t"))

	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	if h.prefix != "" {
		prefixedAttrs := make([]slog.Attr, 0, len(attrs))
		for _, attr := range attrs {
			prefixedAttrs = append(prefixedAttrs, slog.Attr{
				Key:   h.prefix + attr.Key,
				Value: attr.Value,
			})
		}
		attrs = prefixedAttrs
	}

	return &consoleHandler{
		w:      h.w,
		l:      h.l,
		attrs:  append(h.attrs, attrs...),
		prefix: h.prefix,
	}
}

func (h *consoleHandler) WithGroup(group string) slog.Handler {
	if group == "" {
		return h
	}

	attrs := make([]slog.Attr, len(h.attrs))
	copy(attrs, h.attrs)

	return &consoleHandler{
		w:      h.w,
		l:      h.l,
		attrs:  attrs,
		prefix: h.prefix + group + ".",
	}
}
