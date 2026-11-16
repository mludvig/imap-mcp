package diagnostics

import (
	"context"
	"io"
	"log/slog"
	"time"
)

type loggerKey struct{}
type progressKey struct{}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return discardLogger
}

// WithProgress leaves callback serialization to the caller.
func WithProgress(ctx context.Context, progress func(string)) context.Context {
	return context.WithValue(ctx, progressKey{}, progress)
}

func Stage(ctx context.Context, message string) {
	if progress, ok := ctx.Value(progressKey{}).(func(string)); ok && progress != nil {
		progress(message)
	}
	Logger(ctx).DebugContext(ctx, "stage", "stage", message)
}

func Measure(ctx context.Context, stage string) func() {
	start := time.Now()
	return func() {
		Logger(ctx).DebugContext(ctx, "duration", "stage", stage, "duration", time.Since(start))
	}
}
