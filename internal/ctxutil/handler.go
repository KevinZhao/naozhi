package ctxutil

import (
	"context"
	"log/slog"
)

// Correlation attr keys Handler adds, as bits so a derived handler can
// remember which ones With(...) already set.
const (
	hasTrace uint8 = 1 << iota
	hasRun
	hasSession
)

func correlationBit(key string) uint8 {
	switch key {
	case "trace_id":
		return hasTrace
	case "run_id":
		return hasRun
	case "session_key":
		return hasSession
	}
	return 0
}

// Handler wraps a slog.Handler and appends trace_id / run_id / session_key
// from the record's ctx when present and not already on the record or set
// by With(...). Records logged without a ctx (the package-level slog.Info
// family) pass through unchanged. After WithGroup the added attrs nest in
// the group like any record attr; nothing in the tree uses groups today.
type Handler struct {
	inner  slog.Handler
	preset uint8 // correlation keys set by WithAttrs outside any group
	group  bool
}

// NewHandler wraps inner.
func NewHandler(inner slog.Handler) *Handler { return &Handler{inner: inner} }

// Enabled defers to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle adds the correlation attrs the ctx carries, then delegates.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	trace, run, key := TraceID(ctx), RunID(ctx), SessionKey(ctx)
	if trace == "" && run == "" && key == "" {
		return h.inner.Handle(ctx, r)
	}
	have := h.preset
	r.Attrs(func(a slog.Attr) bool {
		have |= correlationBit(a.Key)
		return true
	})
	if trace != "" && have&hasTrace == 0 {
		r.AddAttrs(slog.String("trace_id", trace))
	}
	if run != "" && have&hasRun == 0 {
		r.AddAttrs(slog.String("run_id", run))
	}
	if key != "" && have&hasSession == 0 {
		r.AddAttrs(slog.String("session_key", key))
	}
	return h.inner.Handle(ctx, r)
}

// Unwrap returns the wrapped handler (tests inspect the configured format).
func (h *Handler) Unwrap() slog.Handler { return h.inner }

// WithAttrs wraps the inner handler's WithAttrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	preset := h.preset
	if !h.group {
		for _, a := range attrs {
			preset |= correlationBit(a.Key)
		}
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), preset: preset, group: h.group}
}

// WithGroup wraps the inner handler's WithGroup.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{inner: h.inner.WithGroup(name), preset: h.preset, group: true}
}
