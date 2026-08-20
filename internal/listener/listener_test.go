package listener

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/linyiru/firerunner/internal/core"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestStubJITGenerate(t *testing.T) {
	name, jit, err := StubJIT{}.Generate(context.Background(), core.RunnerSpec{})
	if err != nil || name == "" || jit != "" {
		t.Fatalf("Generate = %q, %q, %v", name, jit, err)
	}
}

func TestStubRunReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewStub(testLogger())
	if err := s.Run(ctx, func(context.Context, int) {}); err == nil {
		t.Fatal("expected context error on cancelled ctx")
	}
}
