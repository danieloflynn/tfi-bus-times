package gtfs

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences production slog output during the gtfs test/benchmark run.
// Several functions (e.g. buildFromZIPFile) log at Info level; during benchmarks
// those lines interleave with the benchmark result lines on the output stream and
// corrupt benchstat parsing. Discarding the logger keeps benchmark output clean.
//
// This changes only where log lines go — no test's behaviour, inputs, or
// assertions are affected.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
