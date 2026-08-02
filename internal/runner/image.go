package runner

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aiofiles/internal/config"
	"aiofiles/internal/jobs"
	"aiofiles/internal/presets"
)

// Image converts and resizes still images with ImageMagick 7.
type Image struct {
	cfg *config.Config
	log *slog.Logger
}

var _ jobs.Runner = (*Image)(nil)

func NewImage(cfg *config.Config, log *slog.Logger) *Image {
	if log == nil {
		log = slog.Default()
	}
	return &Image{cfg: cfg, log: log.With("runner", "magick")}
}

// Run converts j.InputPath into cfg.DownloadDir.
func (r *Image) Run(ctx context.Context, j *jobs.Job, emit jobs.Emit) (jobs.Result, error) {
	var res jobs.Result

	if strings.TrimSpace(j.InputPath) == "" {
		return res, errors.New("magick: job has no input file")
	}
	input := absPath(j.InputPath)
	if st, err := os.Stat(input); err != nil {
		return res, fmt.Errorf("magick: input file: %w", err)
	} else if st.IsDir() {
		return res, errors.New("magick: input path is a directory")
	}

	p, err := presets.ParseImage(j.Params, defaultRetentionDays)
	if err != nil {
		return res, err
	}
	// "Same as the input" only becomes a real format once there is a file.
	format, err := p.ResolveFormat(input)
	if err != nil {
		return res, err
	}

	if err := os.MkdirAll(r.cfg.TmpDir, 0o750); err != nil {
		return res, fmt.Errorf("create tmp dir: %w", err)
	}
	base := filepath.Join(r.cfg.TmpDir, "img-"+sanitizeName(shortID(j.ID)))
	tmpOut := base + "." + sanitizeName(outputExt(format))
	defer os.Remove(tmpOut)

	// docker/policy.xml denies ImageMagick the SVG coder: the renderer it would
	// reach for shares the MVG engine, which reads local files.
	if isSVGPath(input) {
		rasterized := base + "-src.png"
		defer os.Remove(rasterized)
		emitStage(emit, "rendering")
		if err := runProc(ctx, r.log, r.cfg.RsvgBin, rsvgArgs(input, rasterized, p),
			procOpts{}); err != nil {
			return res, err
		}
		if st, err := os.Stat(rasterized); err != nil || st.Size() == 0 {
			return res, errors.New("rsvg-convert produced no output file")
		}
		input = rasterized
	}

	renderFormat, renderPath := format, tmpOut
	switch format {
	case svgFormat:
		renderFormat, renderPath = "png", base+".png"
		defer os.Remove(renderPath)
	case svgTraceFormat:
		renderFormat, renderPath = "pbm", base+".pbm"
		defer os.Remove(renderPath)
	}

	args := magickArgs(input, renderPath, renderFormat, p, renderPath != tmpOut)

	emitStage(emit, "converting")
	if err := runProc(ctx, r.log, r.cfg.MagickBin, args, procOpts{}); err != nil {
		return res, err
	}
	if st, err := os.Stat(renderPath); err != nil || st.Size() == 0 {
		return res, errors.New("magick produced no output file")
	}
	switch format {
	case svgFormat:
		if err := wrapPNGInSVG(renderPath, tmpOut); err != nil {
			return res, err
		}
	case svgTraceFormat:
		emitStage(emit, "tracing")
		args := []string{"--svg", "--output", tmpOut, "--", renderPath}
		if err := runProc(ctx, r.log, r.cfg.PotraceBin, args, procOpts{}); err != nil {
			return res, err
		}
	}

	slug := firstNonEmpty(baseWithoutExt(j.Title), baseWithoutExt(j.Source),
		baseWithoutExt(input), "image")
	dst := uniqueOutputPath(r.cfg.DownloadDir, slug, outputExt(format), j.ID)
	if err := moveFile(tmpOut, dst); err != nil {
		return res, fmt.Errorf("move result: %w", err)
	}

	emitPercent(emit, "done", 100)
	return jobs.Result{OutputPath: dst}, nil
}

// The resource limits come first so a single oversized image cannot exhaust the
// container's memory. They mirror docker/policy.xml, which is the real defence
// but only exists inside the image: memory and map alone just push a
// decompression bomb down into the disk-backed pixel cache under /data/tmp, so
// disk and a wall clock are what actually bound it.
//
// format must be the *resolved* output format: p.Format may still say "source",
// which is not an encoder name and must never reach argv. exactSize drops the
// shrink-only flag from the resize: for an SVG the pixel size is the resolution
// of what gets embedded or traced, so it may go up as well as down.
func magickArgs(input, output, format string, p *presets.ImageParams, exactSize bool) []string {
	args := []string{
		"-limit", "memory", "512MiB",
		"-limit", "map", "1GiB",
		"-limit", "disk", "1GiB",
		"-limit", "area", "128MP",
		"-limit", "width", "32KP",
		"-limit", "height", "32KP",
		// Frames, not pixels: a GIF or TIFF with a million tiny scenes never
		// trips the limits above.
		"-limit", "list-length", "512",
		"-limit", "thread", "2",
		"-limit", "time", "60",
		input,
		"-auto-orient",
	}

	if geom := resizeGeometry(p.Width, p.Height); geom != "" {
		if exactSize {
			geom = strings.TrimSuffix(geom, ">")
		}
		args = append(args, "-resize", geom)
	}
	if p.StripMetadata {
		args = append(args, "-strip")
	}

	switch format {
	case "jpg", "webp", "avif":
		args = append(args, "-quality", strconv.Itoa(p.Quality))
	case "png":
		args = append(args, "-define", "png:compression-level=9")
	case "gif":
		args = append(args, "-layers", "optimize")
	case "pbm":
		// potrace traces a bilevel image; OTSU picks the split from the picture
		// instead of the fixed 50% that flattens a mid-tone photo to one blob.
		args = append(args, "-colorspace", "gray", "-normalize", "-auto-threshold", "OTSU")
		// potrace only ever traces the black side, so tracing the light areas
		// means handing it the negative.
		if p.Trace == "light" {
			args = append(args, "-negate")
		}
	}

	return append(args, output)
}

const (
	svgFormat      = "svg"
	svgTraceFormat = "svg_trace"
)

func outputExt(format string) string {
	if format == svgTraceFormat {
		return svgFormat
	}
	return format
}

func isSVGPath(p string) bool {
	return strings.EqualFold(filepath.Ext(p), ".svg")
}

// --unlimited is deliberately absent: librsvg then refuses oversized documents
// rather than expanding them into a huge surface.
func rsvgArgs(input, output string, p *presets.ImageParams) []string {
	args := []string{"--format=png", "--background-color=none", "--keep-aspect-ratio"}
	if p.Width > 0 {
		args = append(args, "--width", strconv.Itoa(p.Width))
	}
	if p.Height > 0 {
		args = append(args, "--height", strconv.Itoa(p.Height))
	}
	return append(args, "--output", output, "--", input)
}

// wrapPNGInSVG writes an SVG whose only content is src, base64-encoded inline.
// ImageMagick cannot: its SVG writer traces the bitmap through potrace.
// Streamed, because the PNG can be as large as the upload limit.
func wrapPNGInSVG(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("svg: open render: %w", err)
	}
	defer in.Close()

	cfg, err := png.DecodeConfig(in)
	if err != nil {
		return fmt.Errorf("svg: read render size: %w", err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("svg: rewind render: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("svg: create output: %w", err)
	}
	defer out.Close()

	// viewBox without width/height: the file then scales to whatever box it is
	// dropped into, and the pixel size only sets how much detail is in it.
	w := bufio.NewWriter(out)
	if _, err := fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" `+
		`xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 %d %d">`+
		`<image width="%d" height="%d" href="data:image/png;base64,`,
		cfg.Width, cfg.Height, cfg.Width, cfg.Height); err != nil {
		return fmt.Errorf("svg: write: %w", err)
	}
	enc := base64.NewEncoder(base64.StdEncoding, w)
	if _, err := io.Copy(enc, in); err != nil {
		return fmt.Errorf("svg: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("svg: encode: %w", err)
	}
	if _, err := io.WriteString(w, `"/></svg>`+"\n"); err != nil {
		return fmt.Errorf("svg: write: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("svg: write: %w", err)
	}
	return out.Close()
}

// Renders an ImageMagick geometry with the shrink-only ">" flag. An unset
// dimension is left blank so the aspect ratio is preserved.
func resizeGeometry(w, h int) string {
	switch {
	case w > 0 && h > 0:
		return fmt.Sprintf("%dx%d>", w, h)
	case w > 0:
		return fmt.Sprintf("%dx>", w)
	case h > 0:
		return fmt.Sprintf("x%d>", h)
	}
	return ""
}
