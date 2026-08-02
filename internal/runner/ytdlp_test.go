package runner

import (
	"context"
	"strings"
	"sync"
	"testing"

	"aiofiles/internal/jobs"
)

// yt-dlp prints its banners and its progress bar across both stdout and stderr,
// and runProc pumps the two streams concurrently, so the one line handler must
// survive being called from both goroutines at once. Run under -race.
func TestYtDlpLineHandlerSharedAcrossStreams(t *testing.T) {
	sh := requireShell(t)

	var mu sync.Mutex
	var stages []string
	emit := func(p jobs.Progress) {
		mu.Lock()
		defer mu.Unlock()
		stages = append(stages, p.Stage)
	}

	// Both streams write both kinds of line, so the stage is written on one
	// goroutine while the other reads it.
	const script = `i=1
while [ $i -le 300 ]; do
  echo "[Merger] Merging formats"
  echo "[download]  42.3% of 12.34MiB at 1.23MiB/s ETA 00:12"
  echo "[download] Destination: out.mkv" >&2
  echo "[download]  84.6% of 12.34MiB at 1.23MiB/s ETA 00:04" >&2
  i=$((i+1))
done`

	onLine := ytDlpLineHandler(emit)
	if err := runProc(context.Background(), nil, sh, []string{"-c", script},
		procOpts{OnStdout: onLine, OnStderr: onLine}); err != nil {
		t.Fatalf("runProc: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stages) < 1200 {
		t.Fatalf("got %d updates, want one per line", len(stages))
	}
	for _, s := range stages {
		if s != "merging" && s != "downloading" {
			t.Fatalf("unexpected stage %q", s)
		}
	}
}

// The stage a banner announces sticks to the progress lines that follow it.
func TestYtDlpLineHandlerCarriesStage(t *testing.T) {
	var got []jobs.Progress
	onLine := ytDlpLineHandler(func(p jobs.Progress) { got = append(got, p) })

	for _, l := range []string{
		"[download]  10.0% of 1.00MiB at 1.00MiB/s ETA 00:01",
		"[Merger] Merging formats into \"out.mkv\"",
		"[download]  50.0% of 1.00MiB at 1.00MiB/s ETA 00:01",
	} {
		onLine(l)
	}

	want := []string{"downloading", "merging", "merging"}
	if len(got) != len(want) {
		t.Fatalf("got %d updates, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Stage != w {
			t.Errorf("update %d stage = %q, want %q", i, got[i].Stage, w)
		}
	}
	if got[2].Percent != 50 || !strings.Contains(got[2].Speed, "MiB/s") {
		t.Errorf("last update = %+v", got[2])
	}
}
