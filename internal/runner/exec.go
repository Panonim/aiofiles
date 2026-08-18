// Package runner executes the external media tools (yt-dlp, ffmpeg, ImageMagick).
//
// Argv is always built as a slice and handed to exec.CommandContext, never
// assembled as a shell string, and no client-supplied text is ever passed as a
// flag: values come from the allow-lists in internal/presets, from a numeric
// range this package validated, or from a path this package constructed.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"aiofiles/internal/jobs"
)

const (
	// Only the tail reaches the error. A line may be maxLineBytes long, so the
	// line count alone is not a bound - this text lands in SQLite and in SSE.
	maxStderrLines     = 40
	maxStderrLineBytes = 512
	maxStderrBytes     = 4 << 10

	maxSlugLen = 80

	tagLen          = 2
	maxNameAttempts = 16

	// The default 64 KiB scanner limit would abort on some tools' output.
	maxLineBytes = 1 << 20

	// Bounds how long Wait lingers after the process is gone.
	waitDelay = 10 * time.Second
)

type ExitError struct {
	Bin    string
	Code   int
	Stderr string
	err    error
}

func (e *ExitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s failed", filepath.Base(e.Bin))
	if e.Code >= 0 {
		fmt.Fprintf(&b, " (exit %d)", e.Code)
	}
	if e.Stderr != "" {
		b.WriteString(": ")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

func (e *ExitError) Unwrap() error { return e.err }

// ring keeps the last n lines written to it. Safe for concurrent use.
type ring struct {
	mu    sync.Mutex
	buf   []string
	next  int
	full  bool
	limit int
}

func newRing(n int) *ring {
	if n < 1 {
		n = 1
	}
	return &ring{buf: make([]string, n), limit: n}
}

func (r *ring) add(line string) {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return
	}
	if len(line) > maxStderrLineBytes {
		line = strings.ToValidUTF8(line[:maxStderrLineBytes], "") + "..." // cut rune
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = line
	r.next = (r.next + 1) % r.limit
	if r.next == 0 {
		r.full = true
	}
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	if r.full {
		out = append(out, r.buf[r.next:]...)
	}
	out = append(out, r.buf[:r.next]...)
	s := strings.Join(out, "; ")
	if len(s) > maxStderrBytes {
		s = "..." + strings.ToValidUTF8(s[len(s)-maxStderrBytes:], "")
	}
	return s
}

// newCommand puts the process in its own group so the whole tree (ffmpeg spawns
// children, yt-dlp spawns ffmpeg) dies at once when the context is canceled.
func newCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		killGroup(cmd)
		return nil
	}
	return cmd
}

// killGroup SIGKILLs cmd's process group, falling back to the single process
// when the group id cannot be resolved.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}

type procOpts struct {
	Stdout   io.Writer // mutually exclusive with OnStdout
	OnStdout func(line string)
	OnStderr func(line string) // called in addition to the stderr ring buffer
}

// runProc runs bin to completion. A non-zero exit produces an *ExitError
// carrying the tail of stderr.
func runProc(ctx context.Context, log *slog.Logger, bin string, args []string, opts procOpts) error {
	name := filepath.Base(bin)
	cmd := newCommand(ctx, bin, args...)
	stderrRing := newRing(maxStderrLines)

	var stdoutPipe io.ReadCloser
	switch {
	case opts.Stdout != nil:
		cmd.Stdout = opts.Stdout
	case opts.OnStdout != nil:
		p, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("%s: stdout pipe: %w", name, err)
		}
		stdoutPipe = p
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: stderr pipe: %w", name, err)
	}

	if log != nil {
		log.Debug("exec", "bin", bin, "args", args)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: start: %w", name, err)
	}
	// If we bail out before Wait reaps the process, leave no orphaned children.
	reaped := false
	defer func() {
		if !reaped {
			killGroup(cmd)
		}
	}()

	var wg sync.WaitGroup
	if stdoutPipe != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scanLines(stdoutPipe, opts.OnStdout)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanLines(stderrPipe, func(line string) {
			stderrRing.add(line)
			if opts.OnStderr != nil {
				opts.OnStderr(line)
			}
		})
	}()
	// All reads must complete before Wait closes the pipes.
	wg.Wait()

	waitErr := cmd.Wait()
	reaped = true
	if waitErr == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: canceled: %w", name, ctxErr)
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
	}
	return &ExitError{Bin: bin, Code: code, Stderr: stderrRing.String(), err: waitErr}
}

// splitLinesCR treats both "\n" and "\r" as terminators, which is what progress
// output from yt-dlp and ffmpeg needs. Runs of separators are collapsed, so
// empty lines are never reported.
func splitLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0
	for start < len(data) && (data[start] == '\n' || data[start] == '\r') {
		start++
	}
	if j := bytes.IndexAny(data[start:], "\r\n"); j >= 0 {
		return start + j + 1, data[start : start+j], nil
	}
	if atEOF {
		// At EOF the scanner stops as soon as a call returns no token, so the
		// remainder must be emitted now rather than deferred to another round.
		if start < len(data) {
			return len(data), data[start:], nil
		}
		return len(data), nil, nil
	}
	if start > 0 {
		// Drop the separators we skipped so the buffer cannot grow unbounded
		// while we wait for the rest of the line.
		return start, nil, nil
	}
	return 0, nil, nil // ask for more data
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	sc.Split(splitLinesCR)
	return sc
}

// scanLines drains r, invoking fn for every line. Errors are swallowed: the
// process' exit status is the authority on failure, not a truncated pipe.
func scanLines(r io.Reader, fn func(string)) {
	sc := newLineScanner(r)
	for sc.Scan() {
		fn(sc.Text())
	}
}

// capWriter buffers at most max bytes and silently drops the rest.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// sanitizeName reduces s to a filesystem- and argv-safe slug: [A-Za-z0-9._-]
// survives, everything else collapses to a single "-". Never returns "", "."
// or "..".
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		keep := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_'
		if keep {
			b.WriteByte(c)
			dash = false
			continue
		}
		// A literal '-' lands here too, so runs never pile up.
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := b.String()
	if len(out) > maxSlugLen {
		out = out[:maxSlugLen]
	}
	// A leading dash could be read as a flag; a leading dot would hide the file.
	out = strings.Trim(out, "-. \t")
	// Collapse the traversal spellings that survive the filter.
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	if out == "" {
		return "file"
	}
	return out
}

// uniqueOutputPath builds "<dir>/<slug>-<jobID[:2]>.<ext>", redrawing the tag
// when a file of that name is already there: two characters keep names short
// enough to survive being fed back in as a source, but they do collide.
func uniqueOutputPath(dir, base, ext, jobID string) string {
	slug := sanitizeName(base)
	if ext = sanitizeName(strings.TrimPrefix(ext, ".")); ext == "file" {
		ext = ""
	}
	tag := sanitizeName(shortID(jobID))
	for attempt := 0; ; attempt++ {
		name := slug
		if tag != "" && tag != "file" {
			name += "-" + tag
		}
		if ext != "" {
			name += "." + ext
		}
		path := filepath.Join(dir, name)
		if tag == "" || tag == "file" || attempt >= maxNameAttempts {
			return path
		}
		if _, err := os.Stat(path); err != nil {
			return path
		}
		tag = randomTag()
	}
}

func shortID(id string) string {
	if len(id) > tagLen {
		return id[:tagLen]
	}
	return id
}

func randomTag() string {
	var b [tagLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])[:tagLen]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func baseWithoutExt(p string) string {
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// moveFile renames src to dst, falling back to copy+remove when the two live on
// different filesystems (rename returns EXDEV).
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	return copyRemove(src, dst)
}

func copyRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// The copy succeeded; a source we cannot unlink is a cleanup problem, not a
	// failure of the job.
	_ = os.Remove(src)
	return nil
}

// absPath makes p absolute so it can never be mistaken for a flag.
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// emitStage reports a stage change whose percentage is genuinely unknown.
func emitStage(emit jobs.Emit, stage string) {
	if emit != nil {
		emit(jobs.Progress{Percent: -1, Stage: stage})
	}
}

func emitPercent(emit jobs.Emit, stage string, pct float64) {
	if emit == nil {
		return
	}
	emit(jobs.Progress{Percent: clampPercent(pct), Stage: stage})
}

func clampPercent(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return p
}
