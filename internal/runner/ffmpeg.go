package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aiofiles/internal/config"
	"aiofiles/internal/jobs"
	"aiofiles/internal/presets"
)

// FFmpeg serves the TypeConvert, TypeCompress and TypeEdit jobs.
type FFmpeg struct {
	cfg *config.Config
	log *slog.Logger
}

var _ jobs.Runner = (*FFmpeg)(nil)

func NewFFmpeg(cfg *config.Config, log *slog.Logger) *FFmpeg {
	if log == nil {
		log = slog.Default()
	}
	return &FFmpeg{cfg: cfg, log: log.With("runner", "ffmpeg")}
}

// encodeSpec is the union of the convert and compress presets, resolved to the
// values that become argv elements.
type encodeSpec struct {
	Container     string
	VideoCodec    string
	Speed         string
	CRF           int
	Resolution    string
	FrameRate     string
	AudioCodec    string
	AudioBitrate  string
	StripMetadata bool
	Stage         string

	// Set only for a compress job with an explicit size target, which trades
	// CRF for a bitrate budget and therefore needs two passes.
	TargetMB  int
	VideoKbps int
	Pass      int    // 0 = single pass, 1 = analysis, 2 = final
	PassLog   string // -passlogfile base, without ffmpeg's own suffixes
}

const (
	// Margin for container and muxing overhead, so "under N MB" really lands under N MB.
	sizeTargetOverhead = 0.98

	// Below this an encode stops being watchable; a target that cannot clear it
	// is rejected instead of encoded to mush.
	minVideoKbps = 100

	// Guards the arithmetic against an absurd budget (tiny clip, huge target).
	maxVideoKbps = 500_000
)

var videoEncoders = map[string]string{
	"h264": "libx264",
	"h265": "libx265",
	"vp9":  "libvpx-vp9",
	"av1":  "libsvtav1",
	"copy": "copy",
}

// libvpx-vp9's -cpu-used, 0 slowest/best.
var vp9CPUUsed = map[string]int{
	"veryslow": 0, "slow": 1, "medium": 2, "fast": 3, "faster": 4, "veryfast": 5,
}

// libsvtav1's numeric -preset (0..13).
var svtAV1Preset = map[string]int{
	"veryslow": 2, "slow": 4, "medium": 6, "fast": 8, "faster": 9, "veryfast": 10,
}

// Run transcodes j.InputPath into cfg.DownloadDir.
func (r *FFmpeg) Run(ctx context.Context, j *jobs.Job, emit jobs.Emit) (jobs.Result, error) {
	var res jobs.Result

	if strings.TrimSpace(j.InputPath) == "" {
		return res, errors.New("ffmpeg: job has no input file")
	}
	input := absPath(j.InputPath)
	if st, err := os.Stat(input); err != nil {
		return res, fmt.Errorf("ffmpeg: input file: %w", err)
	} else if st.IsDir() {
		return res, errors.New("ffmpeg: input path is a directory")
	}

	if j.Type == jobs.TypeEdit {
		return r.runEdit(ctx, j, input, emit)
	}

	spec, err := r.specFor(j)
	if err != nil {
		return res, err
	}

	// Duration turns out_time_us into a percentage. Missing it is cosmetic for a
	// CRF encode, but a size target has no budget without it.
	emitStage(emit, "probing")
	duration, err := r.probeDuration(ctx, input)
	if err != nil {
		if spec.TargetMB > 0 {
			return res, fmt.Errorf("cannot aim at a file size: the length of this file could not be determined (%w)", err)
		}
		r.log.Debug("ffprobe duration failed", "err", err, "job", j.ID)
		duration = 0
	}
	if spec.TargetMB > 0 {
		if spec.VideoKbps, err = targetVideoKbps(spec.TargetMB, duration, audioKbps(spec)); err != nil {
			return res, err
		}
	}

	if err := os.MkdirAll(r.cfg.TmpDir, 0o750); err != nil {
		return res, fmt.Errorf("create tmp dir: %w", err)
	}
	tmpOut := filepath.Join(r.cfg.TmpDir,
		"enc-"+sanitizeName(shortID(j.ID))+"."+sanitizeName(spec.Container))
	defer os.Remove(tmpOut)

	if spec.VideoKbps > 0 {
		err = r.runTwoPass(ctx, j, input, tmpOut, spec, duration, emit)
	} else {
		err = r.runOnePass(ctx, input, tmpOut, spec, duration, emit)
	}
	if err != nil {
		return res, err
	}
	if st, err := os.Stat(tmpOut); err != nil || st.Size() == 0 {
		return res, errors.New("ffmpeg produced no output file")
	}

	slug := firstNonEmpty(baseWithoutExt(j.Title), baseWithoutExt(j.Source),
		baseWithoutExt(input), "media")
	dst := uniqueOutputPath(r.cfg.DownloadDir, slug, spec.Container, j.ID)
	if err := moveFile(tmpOut, dst); err != nil {
		return res, fmt.Errorf("move result: %w", err)
	}

	emitPercent(emit, "done", 100)
	return jobs.Result{OutputPath: dst}, nil
}

func (r *FFmpeg) runOnePass(ctx context.Context, input, output string, s encodeSpec, duration float64, emit jobs.Emit) error {
	emitStage(emit, s.Stage)
	return r.encode(ctx, ffmpegArgs(input, output, s), &ffProgress{duration: duration, stage: s.Stage}, emit)
}

// runTwoPass drives the bitrate-targeted encode. Pass 1 only writes the analysis
// log ffmpeg needs to spend the budget well, so it owns the first half of the
// progress bar and pass 2 the second: the percentage never walks back.
func (r *FFmpeg) runTwoPass(ctx context.Context, j *jobs.Job, input, output string, s encodeSpec, duration float64, emit jobs.Emit) error {
	logName := "pass-" + sanitizeName(shortID(j.ID))
	s.PassLog = filepath.Join(r.cfg.TmpDir, logName)
	// ffmpeg decorates the base name (-0.log, .log.mbtree, .log.cutree) and
	// leaves the files behind on failure as well as on success.
	defer removePassLogs(r.cfg.TmpDir, logName)

	passes := []struct {
		pass   int
		stage  string
		base   float64
		output string
	}{
		{1, "analyzing", 0, os.DevNull},
		{2, "encoding", 50, output},
	}
	for _, p := range passes {
		s.Pass = p.pass
		emitPercent(emit, p.stage, p.base)
		prog := &ffProgress{duration: duration, stage: p.stage, base: p.base, span: 50}
		if err := r.encode(ctx, ffmpegArgs(input, p.output, s), prog, emit); err != nil {
			return err
		}
	}
	return nil
}

// encode runs one ffmpeg invocation, forwarding its -progress block as updates.
func (r *FFmpeg) encode(ctx context.Context, args []string, prog *ffProgress, emit jobs.Emit) error {
	onLine := func(line string) {
		if p, ok := prog.line(line); ok && emit != nil {
			emit(p)
		}
	}
	return runProc(ctx, r.log, r.cfg.FFmpegBin, args, procOpts{OnStdout: onLine})
}

// runEdit trims and optionally crops. The cut has to land where the user put
// the handles, and a stream copy can only start on a keyframe - seconds out on
// a long GOP - so video is re-encoded at visually lossless quality. Audio is
// copied either way, and a file with no video track is copied whole.
func (r *FFmpeg) runEdit(ctx context.Context, j *jobs.Job, input string, emit jobs.Emit) (jobs.Result, error) {
	var res jobs.Result

	p, err := presets.ParseEdit(j.Params, defaultRetentionDays)
	if err != nil {
		return res, err
	}

	emitStage(emit, "probing")
	span := p.Span()
	if span <= 0 {
		// Only the progress bar needs the length; a failed probe just leaves it blank.
		if total, err := r.probeDuration(ctx, input); err == nil {
			span = total - p.Start
		}
	}

	if err := os.MkdirAll(r.cfg.TmpDir, 0o750); err != nil {
		return res, fmt.Errorf("create tmp dir: %w", err)
	}
	container, copyOnly := editPlan(input)
	tmpOut := filepath.Join(r.cfg.TmpDir, "edit-"+sanitizeName(shortID(j.ID))+"."+container)
	defer os.Remove(tmpOut)

	stage := "trimming"
	if p.HasCrop() {
		stage = "cropping"
	}
	emitStage(emit, stage)
	prog := &ffProgress{duration: span, stage: stage}
	if err := r.encode(ctx, editArgs(input, tmpOut, p, copyOnly), prog, emit); err != nil {
		return res, err
	}
	if st, err := os.Stat(tmpOut); err != nil || st.Size() == 0 {
		return res, errors.New("ffmpeg produced no output file")
	}

	slug := firstNonEmpty(baseWithoutExt(j.Title), baseWithoutExt(j.Source), baseWithoutExt(input), "media")
	dst := uniqueOutputPath(r.cfg.DownloadDir, slug, container, j.ID)
	if err := moveFile(tmpOut, dst); err != nil {
		return res, fmt.Errorf("move result: %w", err)
	}

	emitPercent(emit, "done", 100)
	return jobs.Result{OutputPath: dst}, nil
}

// Extensions with no video track to re-encode, so an edit on one stays a
// lossless copy in its own container.
var audioOnlyExt = map[string]bool{
	"mp3": true, "m4a": true, "aac": true, "flac": true, "wav": true,
	"ogg": true, "oga": true, "opus": true, "wma": true,
}

// editPlan picks the output container and whether the streams can be copied.
// H.264 only belongs in MP4 and MOV among the containers we see, so anything
// else re-encoding lands in MKV rather than failing at the muxer.
func editPlan(input string) (container string, copyOnly bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(input), "."))
	if ext == "" || len(ext) > 5 {
		return "mp4", false
	}
	ext = sanitizeName(ext)
	if audioOnlyExt[ext] {
		return ext, true
	}
	if ext == "mp4" || ext == "mov" || ext == "m4v" {
		return "mp4", false
	}
	return "mkv", false
}

func editArgs(input, output string, p *presets.EditParams, copyOnly bool) []string {
	args := []string{"-hide_banner", "-nostdin", "-y"}
	if p.Start > 0 {
		// Before -i, so ffmpeg seeks to the cut instead of decoding its way there.
		args = append(args, "-ss", seconds(p.Start))
	}
	args = append(args, "-i", input, "-progress", "pipe:1", "-nostats")
	if span := p.Span(); span > 0 {
		args = append(args, "-t", seconds(span))
	}

	if copyOnly {
		args = append(args, "-c", "copy", "-avoid_negative_ts", "make_zero")
	} else {
		if p.HasCrop() {
			args = append(args, "-vf",
				fmt.Sprintf("crop=%d:%d:%d:%d", p.CropW, p.CropH, p.CropX, p.CropY))
		}
		// CRF 18 is visually lossless; the audio is passed through untouched.
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "18",
			"-pix_fmt", "yuv420p", "-c:a", "copy")
	}

	if strings.HasSuffix(output, ".mp4") || strings.HasSuffix(output, ".mov") {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, output)
}

func seconds(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

// Matching by prefix rather than glob keeps a metacharacter in the tmp path from
// turning into a pattern.
func removePassLogs(dir, prefix string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// audioKbps is the share of a size budget the audio track will claim.
func audioKbps(s encodeSpec) int {
	if s.AudioCodec == "none" {
		return 0
	}
	n, _ := strconv.Atoi(s.AudioBitrate) // allow-listed, always numeric
	return n
}

// targetVideoKbps divides a target file size over the running time, leaving room
// for audio and the container. Megabytes are 1,000,000 bytes: undershooting a
// limit is harmless, overshooting defeats the point of asking for one.
func targetVideoKbps(targetMB int, durationSec float64, audioKbps int) (int, error) {
	if durationSec <= 0 {
		return 0, errors.New("cannot aim at a file size: the length of this file is unknown")
	}
	totalKbps := float64(targetMB) * 1e6 * 8 / 1000 / durationSec * sizeTargetOverhead
	video := int(totalKbps) - audioKbps
	if video < minVideoKbps {
		return 0, fmt.Errorf(
			"a %d MB target cannot hold %s of video at %d kbps audio - raise the target size, lower the audio bitrate, or trim the file",
			targetMB, humanDuration(durationSec), audioKbps)
	}
	return min(video, maxVideoKbps), nil
}

// humanDuration renders a running time for an error message a user reads.
func humanDuration(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d hours %d minutes", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%d minutes %d seconds", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		return fmt.Sprintf("%d seconds", int(d/time.Second))
	}
}

func (r *FFmpeg) specFor(j *jobs.Job) (encodeSpec, error) {
	switch j.Type {
	case jobs.TypeConvert:
		p, err := presets.ParseConvert(j.Params, defaultRetentionDays)
		if err != nil {
			return encodeSpec{}, err
		}
		speed, crf := p.Resolved()
		return encodeSpec{
			Container:     p.Container,
			VideoCodec:    p.VideoCodec,
			Speed:         speed,
			CRF:           crf,
			Resolution:    p.Resolution,
			FrameRate:     p.FrameRate,
			AudioCodec:    p.AudioCodec,
			AudioBitrate:  p.AudioBitrate,
			StripMetadata: p.StripMetadata,
			Stage:         "converting",
		}, nil

	case jobs.TypeCompress:
		p, err := presets.ParseCompress(j.Params, defaultRetentionDays)
		if err != nil {
			return encodeSpec{}, err
		}
		return encodeSpec{
			Container:     p.Container,
			VideoCodec:    p.VideoCodec,
			Speed:         p.Speed,
			CRF:           p.CompressCRFFor(),
			Resolution:    p.Resolution,
			FrameRate:     "source",
			AudioCodec:    presets.DefaultAudioCodec(p.Container),
			AudioBitrate:  p.AudioBitrate,
			StripMetadata: p.StripMetadata,
			Stage:         "encoding",
			TargetMB:      p.TargetSizeMB,
		}, nil
	}
	return encodeSpec{}, fmt.Errorf("ffmpeg: unsupported job type %q", j.Type)
}

func ffmpegArgs(input, output string, s encodeSpec) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", input,
		"-progress", "pipe:1", "-nostats",
	}

	enc := videoEncoders[s.VideoCodec]
	if enc == "" {
		enc = "libx264" // unreachable: the preset table is closed
	}
	args = append(args, "-c:v", enc)

	if enc != "copy" {
		switch enc {
		case "libvpx-vp9":
			args = append(args, "-cpu-used", strconv.Itoa(vp9CPUUsed[s.Speed]), "-row-mt", "1")
		case "libsvtav1":
			args = append(args, "-preset", strconv.Itoa(svtAV1Preset[s.Speed]))
		default:
			args = append(args, "-preset", s.Speed)
		}
		args = append(args, rateControl(enc, s)...)

		// Filters and frame-rate conversion require a re-encode; with -c:v copy
		// ffmpeg would refuse them.
		if s.Resolution != "" && s.Resolution != "source" {
			if h, err := strconv.Atoi(s.Resolution); err == nil && h > 0 {
				args = append(args, "-vf", "scale=-2:"+strconv.Itoa(h))
			}
		}
		if s.FrameRate != "" && s.FrameRate != "source" {
			if fps, err := strconv.Atoi(s.FrameRate); err == nil && fps > 0 {
				args = append(args, "-r", strconv.Itoa(fps))
			}
		}
	}

	if s.Pass == 1 {
		// Pass 1 only fills the analysis log; audio and a real container would be wasted work.
		return append(args, "-an", "-f", "null", output)
	}

	switch s.AudioCodec {
	case "none":
		args = append(args, "-an")
	case "copy":
		args = append(args, "-c:a", "copy")
	case "flac":
		args = append(args, "-c:a", "flac")
	case "aac":
		args = append(args, "-c:a", "aac", "-b:a", s.AudioBitrate+"k")
	case "opus":
		args = append(args, "-c:a", "libopus", "-b:a", s.AudioBitrate+"k")
	case "mp3":
		args = append(args, "-c:a", "libmp3lame", "-b:a", s.AudioBitrate+"k")
	}

	if s.StripMetadata {
		args = append(args, "-map_metadata", "-1")
	}
	if s.Container == "mp4" || s.Container == "mov" {
		args = append(args, "-movflags", "+faststart")
	}

	return append(args, output)
}

// A size target has to be a two-pass -b:v encode: CRF cannot promise a file size.
func rateControl(enc string, s encodeSpec) []string {
	if s.Pass > 0 {
		return []string{
			"-b:v", strconv.Itoa(s.VideoKbps) + "k",
			"-pass", strconv.Itoa(s.Pass),
			"-passlogfile", s.PassLog,
		}
	}
	if enc == "libvpx-vp9" {
		// libvpx treats a non-zero -b:v as a cap, which would override the CRF.
		return []string{"-crf", strconv.Itoa(s.CRF), "-b:v", "0"}
	}
	return []string{"-crf", strconv.Itoa(s.CRF)}
}

// probeDuration returns the container duration in seconds.
func (r *FFmpeg) probeDuration(ctx context.Context, input string) (float64, error) {
	out := &capWriter{max: 4096}
	args := []string{"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", "--", input}
	if err := runProc(ctx, r.log, r.cfg.FFprobeBin, args, procOpts{Stdout: out}); err != nil {
		return 0, err
	}
	txt := strings.TrimSpace(out.buf.String())
	if txt == "" || txt == "N/A" {
		return 0, errors.New("duration unavailable")
	}
	if i := strings.IndexAny(txt, "\r\n"); i >= 0 {
		txt = txt[:i]
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(txt), 64)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("unparsable duration %q", txt)
	}
	return d, nil
}

// ffProgress accumulates the key=value block ffmpeg writes to -progress and
// emits one update per block.
type ffProgress struct {
	duration float64 // seconds, 0 when unknown
	stage    string
	base     float64 // percentage this run starts at (a two-pass encode splits the bar)
	span     float64 // percentage width this run covers; 0 means the whole bar
	outUS    float64
	speed    string
	haveTime bool
}

// scale maps a 0..1 position within this run onto the overall percentage.
func (f *ffProgress) scale(frac float64) float64 {
	span := f.span
	if span <= 0 {
		span = 100
	}
	return clampPercent(f.base + frac*span)
}

// line feeds one "key=value" line and reports a Progress when a block ends.
func (f *ffProgress) line(s string) (jobs.Progress, bool) {
	k, v, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok {
		return jobs.Progress{}, false
	}
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)

	switch k {
	case "out_time_us", "out_time_ms":
		// ffmpeg's out_time_ms is microseconds too (a long-standing quirk).
		if v != "N/A" {
			if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
				f.outUS, f.haveTime = n, true
			}
		}
	case "speed":
		if v != "N/A" && v != "0x" {
			f.speed = v
		}
	case "progress":
		p := jobs.Progress{Percent: -1, Stage: f.stage, Speed: f.speed}
		if v == "end" {
			p.Percent = f.scale(1)
			return p, true
		}
		if f.duration > 0 && f.haveTime {
			p.Percent = f.scale(f.outUS / (f.duration * 1e6))
		}
		return p, true
	}
	return jobs.Progress{}, false
}
