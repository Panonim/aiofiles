package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "file"},
		{"dot", ".", "file"},
		{"dot dot", "..", "file"},
		{"traversal", "../../etc/passwd", "etc-passwd"},
		{"windows traversal", `..\..\windows\system32`, "windows-system32"},
		{"absolute path", "/etc/passwd", "etc-passwd"},
		{"root", "/", "file"},
		{"leading dash", "-rf", "rf"},
		{"flag lookalike", "--output=/tmp/x", "output-tmp-x"},
		{"leading dot", ".hidden", "hidden"},
		{"only separators", "///", "file"},
		{"only dashes", "----", "file"},
		{"whitespace", "   ", "file"},
		{"inner dots kept", "a.b.c", "a.b.c"},
		{"double dot collapsed", "file..name", "file.name"},
		{"shell substitution", "$(rm -rf /)", "rm-rf"},
		{"pipe and redirect", "a|b>c", "a-b-c"},
		{"newline", "line1\nline2", "line1-line2"},
		{"nul byte", "video\x00name", "video-name"},
		{"spaces and parens", "My Video (1080p).mp4", "My-Video-1080p-.mp4"},
		{"underscores survive", "my_video_2024.mkv", "my_video_2024.mkv"},
		{"unicode", "日本語のビデオ.mp4", "mp4"},
		{"accented", "café.mp4", "caf-.mp4"},
		{"collapses to nothing after truncation", strings.Repeat(".", 100) + "x", "file"},
		{"truncated to the slug limit", strings.Repeat("a", 300), strings.Repeat("a", maxSlugLen)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertSafeSlug(t, tc.in, got)
		})
	}
}

// The invariants above are the actual security contract, so hold every input to
// them regardless of the exact expected string.
func assertSafeSlug(t *testing.T, in, got string) {
	t.Helper()
	switch got {
	case "", ".", "..":
		t.Fatalf("sanitizeName(%q) = %q", in, got)
	}
	if strings.HasPrefix(got, "-") {
		t.Errorf("sanitizeName(%q) = %q starts with a dash", in, got)
	}
	if strings.HasPrefix(got, ".") {
		t.Errorf("sanitizeName(%q) = %q starts with a dot", in, got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("sanitizeName(%q) = %q contains a traversal", in, got)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("sanitizeName(%q) = %q contains a path separator", in, got)
	}
	if len(got) > maxSlugLen {
		t.Errorf("sanitizeName(%q) = %q is %d bytes, over the %d limit", in, got, len(got), maxSlugLen)
	}
	for i := 0; i < len(got); i++ {
		c := got[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			t.Fatalf("sanitizeName(%q) = %q contains byte %q", in, got, c)
		}
	}
}

func TestSanitizeNameHostileInputsStayContained(t *testing.T) {
	hostile := []string{
		"../" + strings.Repeat("../", 40) + "etc/shadow",
		strings.Repeat("..", 200),
		strings.Repeat("-", 200) + "x",
		"\x00\x01\x02",
		"....//....//etc",
		"~/.ssh/authorized_keys",
		"con:\\aux",
		strings.Repeat("é", 200),
	}
	for _, in := range hostile {
		got := sanitizeName(in)
		assertSafeSlug(t, in, got)
		// The slug is joined onto a directory, so it must never climb out of it.
		joined := filepath.Join("/data/downloads", got)
		if filepath.Dir(joined) != "/data/downloads" {
			t.Errorf("sanitizeName(%q) = %q escapes its directory: %q", in, got, joined)
		}
	}
}

func TestUniqueOutputPath(t *testing.T) {
	const dir = "/data/downloads"
	const id = "abcdef0123456789"

	tests := []struct {
		name             string
		base, ext, jobID string
		want             string
	}{
		{"plain", "My Video", "mp4", id, "/data/downloads/My-Video-abcdef01.mp4"},
		{"extension with dot", "My Video", ".mkv", id, "/data/downloads/My-Video-abcdef01.mkv"},
		{"no extension", "clip", "", id, "/data/downloads/clip-abcdef01"},
		{"short job id", "clip", "mp4", "abc", "/data/downloads/clip-abc.mp4"},
		{"no job id", "clip", "mp4", "", "/data/downloads/clip.mp4"},
		{"empty base", "", "mp4", id, "/data/downloads/file-abcdef01.mp4"},
		{"traversal base", "../../etc/passwd", "mp4", id, "/data/downloads/etc-passwd-abcdef01.mp4"},
		{"traversal extension", "clip", "./../sh", id, "/data/downloads/clip-abcdef01.sh"},
		// A job id that sanitises to nothing is dropped rather than spelled "file".
		{"hostile job id", "clip", "mp4", "../../../x", "/data/downloads/clip.mp4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueOutputPath(dir, tc.base, tc.ext, tc.jobID)
			if got != tc.want {
				t.Errorf("uniqueOutputPath(%q, %q, %q, %q) = %q, want %q",
					dir, tc.base, tc.ext, tc.jobID, got, tc.want)
			}
			if filepath.Dir(got) != dir {
				t.Errorf("output %q left the download directory", got)
			}
			if strings.HasPrefix(filepath.Base(got), "-") {
				t.Errorf("output basename %q starts with a dash", filepath.Base(got))
			}
		})
	}
}

func TestLineScannerSplitsOnCRAndLF(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single line without terminator", "hello", []string{"hello"}},
		{"unix lines", "a\nb\nc\n", []string{"a", "b", "c"}},
		// ffmpeg and yt-dlp redraw progress with a bare carriage return.
		{"carriage returns", "10%\r20%\r30%\r", []string{"10%", "20%", "30%"}},
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"mixed", "a\rb\nc\r\nd", []string{"a", "b", "c", "d"}},
		{"leading separators", "\r\n\n\ra\n", []string{"a"}},
		{"runs collapse to no empty lines", "a\n\n\n\r\r\nb\n", []string{"a", "b"}},
		{"only separators", "\r\n\r\n", nil},
		{"trailing separators", "a\n\r\n", []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanAll(t, strings.NewReader(tc.in)); !equalStrings(got, tc.want) {
				t.Errorf("scan(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// A pipe hands over arbitrary chunks; the split must not depend on
			// a whole line arriving in one read.
			got := scanAll(t, iotest.OneByteReader(strings.NewReader(tc.in)))
			if !equalStrings(got, tc.want) {
				t.Errorf("byte-at-a-time scan(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The default bufio limit is 64 KiB and some tools emit far more per line.
func TestLineScannerHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 512*1024)
	got := scanAll(t, strings.NewReader(long+"\nshort\n"))
	if len(got) != 2 || len(got[0]) != len(long) || got[1] != "short" {
		t.Fatalf("got %d lines, first of %d bytes; want 2 lines, first of %d",
			len(got), len(got[0]), len(long))
	}
}

func scanAll(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := newLineScanner(r)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRing(t *testing.T) {
	r := newRing(3)
	if got := r.String(); got != "" {
		t.Errorf("empty ring = %q", got)
	}
	for _, l := range []string{"one", "two  ", "", "three", "four"} {
		r.add(l)
	}
	if got := r.String(); got != "two; three; four" {
		t.Errorf("ring = %q, want %q", got, "two; three; four")
	}
}

func TestRingCapsSize(t *testing.T) {
	r := newRing(1)
	r.add(strings.Repeat("x", 4*maxStderrLineBytes))
	got := r.String()
	if len(got) > maxStderrLineBytes+8 {
		t.Errorf("single line = %d bytes, want <= %d", len(got), maxStderrLineBytes+8)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated line = %q, want a ... suffix", got)
	}

	r = newRing(maxStderrLines)
	for i := 0; i < maxStderrLines; i++ {
		r.add(strings.Repeat("y", maxStderrLineBytes))
	}
	if got := r.String(); len(got) > maxStderrBytes+8 {
		t.Errorf("tail = %d bytes, want <= %d", len(got), maxStderrBytes+8)
	}
}

func requireShell(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sh", "/usr/bin/sh"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	t.Skip("no POSIX shell available")
	return ""
}

func TestRunProc(t *testing.T) {
	sh := requireShell(t)

	t.Run("collects stdout lines", func(t *testing.T) {
		var mu sync.Mutex
		var lines []string
		err := runProc(context.Background(), nil, sh, []string{"-c", "printf 'a\\rb\\nc\\n'"},
			procOpts{OnStdout: func(l string) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, l)
			}})
		if err != nil {
			t.Fatalf("runProc: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !equalStrings(lines, []string{"a", "b", "c"}) {
			t.Errorf("lines = %q", lines)
		}
	})

	t.Run("writes stdout to a writer", func(t *testing.T) {
		out := &capWriter{max: 4}
		if err := runProc(context.Background(), nil, sh, []string{"-c", "printf 'abcdefgh'"},
			procOpts{Stdout: out}); err != nil {
			t.Fatalf("runProc: %v", err)
		}
		if got := out.buf.String(); got != "abcd" {
			t.Errorf("capWriter kept %q, want %q", got, "abcd")
		}
	})

	t.Run("non-zero exit carries the stderr tail", func(t *testing.T) {
		script := `i=1; while [ $i -le 100 ]; do echo line$i >&2; i=$((i+1)); done; exit 3`
		err := runProc(context.Background(), nil, sh, []string{"-c", script}, procOpts{})
		var ee *ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("err = %v (%T), want *ExitError", err, err)
		}
		if ee.Code != 3 {
			t.Errorf("exit code = %d, want 3", ee.Code)
		}
		if !strings.Contains(ee.Stderr, "line100") {
			t.Errorf("stderr tail is missing the last line: %q", ee.Stderr)
		}
		if strings.Contains(ee.Stderr, "line50") {
			t.Errorf("stderr tail kept more than %d lines: %q", maxStderrLines, ee.Stderr)
		}
		if !strings.Contains(ee.Error(), "exit 3") {
			t.Errorf("error text = %q", ee.Error())
		}
	})

	t.Run("cancellation kills the process tree", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		start := time.Now()
		err := runProc(ctx, nil, sh, []string{"-c", "echo ready; sleep 60"},
			procOpts{OnStdout: func(l string) {
				if strings.TrimSpace(l) == "ready" {
					cancel()
				}
			}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("cancel took %s; the child was not killed promptly", elapsed)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		err := runProc(context.Background(), nil,
			filepath.Join(t.TempDir(), "definitely-not-installed"), nil, procOpts{})
		if err == nil {
			t.Fatal("expected an error for a missing binary")
		}
		var ee *ExitError
		if errors.As(err, &ee) {
			t.Errorf("a failure to start should not look like a non-zero exit: %v", err)
		}
	})
}
