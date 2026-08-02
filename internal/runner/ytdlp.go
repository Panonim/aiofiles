package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiofiles/internal/config"
	"aiofiles/internal/jobs"
	"aiofiles/internal/presets"
)

const (
	// A wedged extractor must not pin a request handler forever.
	probeTimeout = 45 * time.Second

	// Cap on the JSON we are willing to buffer from `yt-dlp -J`.
	maxProbeBytes = 32 << 20

	// Fallback when a job's params omit retention. Must stay one of the values
	// presets.checkRetention accepts.
	defaultRetentionDays = 7
)

// MediaInfo is the trimmed-down view of a source URL that the UI needs.
type MediaInfo struct {
	Title      string   `json:"title"`
	Uploader   string   `json:"uploader"`
	Duration   float64  `json:"duration"` // seconds
	Thumbnail  string   `json:"thumbnail"`
	WebpageURL string   `json:"webpage_url"`
	Extractor  string   `json:"extractor"`
	IsPlaylist bool     `json:"is_playlist"`
	Formats    []Format `json:"formats"`
}

// Format is one downloadable stream offered by the extractor.
type Format struct {
	ID         string  `json:"id"`
	Ext        string  `json:"ext"`
	Resolution string  `json:"resolution"` // e.g. "1920x1080" or "audio only"
	FPS        float64 `json:"fps"`
	VCodec     string  `json:"vcodec"`
	ACodec     string  `json:"acodec"`
	Filesize   int64   `json:"filesize"` // bytes, 0 if unknown
	Bitrate    float64 `json:"bitrate"`  // tbr, kbps
	Note       string  `json:"note"`
	HasVideo   bool    `json:"has_video"`
	HasAudio   bool    `json:"has_audio"`
}

type YtDlp struct {
	cfg *config.Config
	log *slog.Logger
}

var _ jobs.Runner = (*YtDlp)(nil)

func NewYtDlp(cfg *config.Config, log *slog.Logger) *YtDlp {
	if log == nil {
		log = slog.Default()
	}
	return &YtDlp{cfg: cfg, log: log.With("runner", "yt-dlp")}
}

// rawInfo mirrors the subset of `yt-dlp -J` output we consume.
type rawInfo struct {
	Type        string      `json:"_type"`
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Uploader    string      `json:"uploader"`
	Channel     string      `json:"channel"`
	UploaderID  string      `json:"uploader_id"`
	Duration    float64     `json:"duration"`
	Thumbnail   string      `json:"thumbnail"`
	WebpageURL  string      `json:"webpage_url"`
	Extractor   string      `json:"extractor_key"`
	ExtractorLC string      `json:"extractor"`
	Formats     []rawFormat `json:"formats"`
	Entries     []rawInfo   `json:"entries"`
}

type rawFormat struct {
	FormatID       string  `json:"format_id"`
	Ext            string  `json:"ext"`
	Resolution     string  `json:"resolution"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	FPS            float64 `json:"fps"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
	TBR            float64 `json:"tbr"`
	FormatNote     string  `json:"format_note"`
}

func (r *YtDlp) Probe(ctx context.Context, rawURL string) (*MediaInfo, error) {
	u, err := presets.ValidateSourceURL(rawURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out := &capWriter{max: maxProbeBytes}
	args := []string{"-J", "--no-warnings", "--no-playlist", "--no-download", "--", u}
	if err := runProc(ctx, r.log, r.cfg.YtDlpBin, args, procOpts{Stdout: out}); err != nil {
		return nil, fmt.Errorf("probe %s: %w", redactURL(u), err)
	}

	var info rawInfo
	if err := json.Unmarshal(out.buf.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("probe %s: parse metadata: %w", redactURL(u), err)
	}
	return buildMediaInfo(&info), nil
}

func buildMediaInfo(info *rawInfo) *MediaInfo {
	mi := &MediaInfo{
		Title:      info.Title,
		Uploader:   firstNonEmpty(info.Uploader, info.Channel, info.UploaderID),
		Duration:   info.Duration,
		Thumbnail:  info.Thumbnail,
		WebpageURL: info.WebpageURL,
		Extractor:  firstNonEmpty(info.Extractor, info.ExtractorLC),
	}

	formats := info.Formats
	if info.Type == "playlist" || info.Type == "multi_video" {
		mi.IsPlaylist = true
		// v1: describe the playlist through its first entry.
		if len(info.Entries) > 0 {
			e := info.Entries[0]
			formats = e.Formats
			if mi.Title == "" {
				mi.Title = e.Title
			}
			if mi.Duration == 0 {
				mi.Duration = e.Duration
			}
			if mi.Thumbnail == "" {
				mi.Thumbnail = e.Thumbnail
			}
			if mi.Uploader == "" {
				mi.Uploader = firstNonEmpty(e.Uploader, e.Channel, e.UploaderID)
			}
		}
	}

	sortRawFormats(formats)
	mi.Formats = make([]Format, 0, len(formats))
	for _, f := range formats {
		mi.Formats = append(mi.Formats, convertFormat(f))
	}
	return mi
}

func convertFormat(f rawFormat) Format {
	hasVideo := f.VCodec != "" && f.VCodec != "none"
	hasAudio := f.ACodec != "" && f.ACodec != "none"

	res := f.Resolution
	if res == "" {
		switch {
		case f.Width > 0 && f.Height > 0:
			res = fmt.Sprintf("%dx%d", f.Width, f.Height)
		case f.Height > 0:
			res = fmt.Sprintf("%dp", f.Height)
		case !hasVideo && hasAudio:
			res = "audio only"
		}
	}

	size := f.Filesize
	if size == 0 {
		size = f.FilesizeApprox
	}
	if size < 0 {
		size = 0
	}

	return Format{
		ID:         f.FormatID,
		Ext:        f.Ext,
		Resolution: res,
		FPS:        f.FPS,
		VCodec:     f.VCodec,
		ACodec:     f.ACodec,
		Filesize:   size,
		Bitrate:    f.TBR,
		Note:       f.FormatNote,
		HasVideo:   hasVideo,
		HasAudio:   hasAudio,
	}
}

// sortRawFormats puts video formats first (tallest first, then bitrate), then
// audio-only formats by bitrate.
func sortRawFormats(fs []rawFormat) {
	video := func(f rawFormat) bool { return f.VCodec != "" && f.VCodec != "none" }
	height := func(f rawFormat) int {
		if f.Height > 0 {
			return f.Height
		}
		// Fall back to "1920x1080" style strings.
		if i := strings.IndexByte(f.Resolution, 'x'); i > 0 {
			if h, err := strconv.Atoi(f.Resolution[i+1:]); err == nil {
				return h
			}
		}
		return 0
	}
	sort.SliceStable(fs, func(i, j int) bool {
		vi, vj := video(fs[i]), video(fs[j])
		if vi != vj {
			return vi // video before audio-only
		}
		if vi {
			if hi, hj := height(fs[i]), height(fs[j]); hi != hj {
				return hi > hj
			}
		}
		return fs[i].TBR > fs[j].TBR
	})
}

// Query strings may carry tokens, so they never reach error text.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i > 0 {
		return u[:i]
	}
	return u
}

// Run downloads j.Source into cfg.DownloadDir.
func (r *YtDlp) Run(ctx context.Context, j *jobs.Job, emit jobs.Emit) (jobs.Result, error) {
	var res jobs.Result

	p, err := presets.ParseDownload(j.Params, defaultRetentionDays)
	if err != nil {
		return res, err
	}
	srcURL, err := presets.ValidateSourceURL(j.Source)
	if err != nil {
		return res, err
	}

	jobDir := filepath.Join(r.cfg.TmpDir, "dl-"+sanitizeName(shortID(j.ID)))
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		return res, fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(jobDir)

	args := ytDlpArgs(p, filepath.Join(jobDir, "%(title).80B-%(id)s.%(ext)s"), srcURL)

	emitStage(emit, "downloading")
	onLine := ytDlpLineHandler(emit)
	if err := runProc(ctx, r.log, r.cfg.YtDlpBin, args, procOpts{OnStdout: onLine, OnStderr: onLine}); err != nil {
		return res, err
	}

	title, produced, err := collectDownload(jobDir)
	if err != nil {
		return res, err
	}

	slug := firstNonEmpty(title, baseWithoutExt(produced), j.Title, "download")
	dst := uniqueOutputPath(r.cfg.DownloadDir, slug, filepath.Ext(produced), j.ID)
	if err := moveFile(produced, dst); err != nil {
		return res, fmt.Errorf("move result: %w", err)
	}

	emitPercent(emit, "done", 100)
	return jobs.Result{OutputPath: dst, Title: title}, nil
}

// ytDlpLineHandler returns the line callback used for both stdout and stderr.
// runProc pumps the two streams in separate goroutines, so the stage carried
// from a banner line to the progress lines that follow needs a lock.
func ytDlpLineHandler(emit jobs.Emit) func(string) {
	var mu sync.Mutex
	stage := "downloading"
	return func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if s, ok := ytDlpStage(line); ok {
			stage = s
			emitStage(emit, stage)
			return
		}
		if pr, ok := parseYtDlpProgress(line); ok {
			pr.Stage = stage
			if emit != nil {
				emit(pr)
			}
		}
	}
}

func ytDlpArgs(p *presets.DownloadParams, tmpl, url string) []string {
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--newline",
		"--progress",
		"--no-part",
		"--restrict-filenames",
		"--write-info-json", // the only way to learn the real title without a second round trip
		"-o", tmpl,
	}

	if p.Mode == "audio" {
		args = append(args, "-x", "--audio-format", p.AudioFormat)
		if p.AudioBitrate != "" {
			args = append(args, "--audio-quality", p.AudioBitrate+"K")
		}
	} else {
		args = append(args, "-f", videoFormatSelector(p.FormatID))
		if p.Container != "" {
			args = append(args, "--merge-output-format", p.Container)
		}
	}

	if p.EmbedSubs {
		args = append(args, "--embed-subs")
	}
	if p.EmbedThumb {
		args = append(args, "--embed-thumbnail")
	}
	if p.EmbedMetadata {
		args = append(args, "--embed-metadata")
	}

	return append(args, "--", url)
}

// videoFormatSelector makes sure a picked format ends up with an audio track.
// DASH sites list high-resolution streams as video only, so a bare "-f 137"
// yields a silent file; "+ba" merges the best audio in. --no-audio-multistreams
// is yt-dlp's default, so a progressive format gains no second track, and the
// "/id" fallback covers sources with no separate audio stream at all.
func videoFormatSelector(id string) string {
	switch {
	case id == "":
		return "bv*+ba/b"
	case strings.Contains(id, "+"):
		return id // the caller already asked for an explicit merge
	default:
		return id + "+ba/" + id
	}
}

var (
	ytPercentRe = regexp.MustCompile(`\[download\]\s+(\d+(?:\.\d+)?)%`)
	ytSpeedRe   = regexp.MustCompile(`\sat\s+(.+?)(?:\s+ETA\s|\s+in\s|\s*$)`)
	ytETARe     = regexp.MustCompile(`\sETA\s+(\S+)`)
)

// Parses lines like "[download]  42.3% of 12.34MiB at 1.23MiB/s ETA 00:12".
func parseYtDlpProgress(line string) (jobs.Progress, bool) {
	m := ytPercentRe.FindStringSubmatch(line)
	if m == nil {
		return jobs.Progress{}, false
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return jobs.Progress{}, false
	}
	pr := jobs.Progress{Percent: clampPercent(pct)}
	if s := ytSpeedRe.FindStringSubmatch(line); s != nil {
		if v := strings.TrimSpace(s[1]); v != "" && !strings.HasPrefix(v, "Unknown") {
			pr.Speed = v
		}
	}
	if e := ytETARe.FindStringSubmatch(line); e != nil {
		if v := e[1]; v != "Unknown" && v != "NA" {
			pr.ETA = v
		}
	}
	return pr, true
}

// ytDlpStage recognises the post-processor banners yt-dlp prints between phases,
// so the UI can show what is happening after the bytes are in.
func ytDlpStage(line string) (string, bool) {
	switch {
	case strings.HasPrefix(line, "[Merger]"):
		return "merging", true
	case strings.HasPrefix(line, "[ExtractAudio]"),
		strings.HasPrefix(line, "[EmbedThumbnail]"),
		strings.HasPrefix(line, "[Metadata]"),
		strings.HasPrefix(line, "[EmbedSubtitle]"),
		strings.HasPrefix(line, "[VideoConvertor]"),
		strings.HasPrefix(line, "[VideoRemuxer]"):
		return "converting", true
	case strings.HasPrefix(line, "[download] Destination:"):
		return "downloading", true
	}
	return "", false
}

// collectDownload finds the media file yt-dlp produced in dir and reads the title
// out of the sidecar info JSON. JSON and partial artefacts are skipped; if
// several candidates remain the largest wins.
func collectDownload(dir string) (title, path string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("read work dir: %w", err)
	}

	var best string
	var bestSize int64 = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".ytdl") ||
			strings.HasSuffix(lower, ".temp") {
			continue
		}
		full := filepath.Join(dir, name)
		if strings.HasSuffix(lower, ".info.json") {
			if t := readInfoJSONTitle(full); t != "" {
				title = t
			}
			continue
		}
		if strings.HasSuffix(lower, ".json") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.Size() > bestSize {
			best, bestSize = full, fi.Size()
		}
	}
	if best == "" {
		return "", "", errors.New("yt-dlp produced no output file")
	}
	return title, best, nil
}

func readInfoJSONTitle(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var info rawInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return ""
	}
	return info.Title
}
