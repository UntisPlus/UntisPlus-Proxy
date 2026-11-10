package main

import (
	"os"
	"strings"
	"testing"

	"untis-proxy/internal/proxy"
)

func TestProgressBar(t *testing.T) {
	if got := progressBar(0, 10); got != "----------" {
		t.Fatalf("0%% = %q", got)
	}
	if got := progressBar(100, 10); got != "==========" {
		t.Fatalf("100%% = %q", got)
	}
	if got := progressBar(50, 10); got != "=====-----" {
		t.Fatalf("50%% = %q", got)
	}
	if got := progressBar(150, 4); got != "====" {
		t.Fatalf("clamped = %q", got)
	}
}

func TestReconPrinterReportsPercentage(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "progress-*.txt")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer f.Close()
	print := newReconPrinter(f, proxy.ReconHard)
	print(proxy.ReconProgress{ChunksDone: 0, ChunksTotal: 10, ClassesTotal: 4})
	print(proxy.ReconProgress{ChunksDone: 5, ChunksTotal: 10, ClassesDone: 2, ClassesTotal: 4})
	print(proxy.ReconProgress{ChunksDone: 10, ChunksTotal: 10, ClassesDone: 4, ClassesTotal: 4})
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	body, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(body)
	for _, want := range []string{"50%", "100%", "2/4 classes", "10/10 chunks"} {
		if !strings.Contains(out, want) {
			t.Fatalf("progress output missing %q:\n%s", want, out)
		}
	}
}
