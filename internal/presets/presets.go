// Package presets defines the allow-listed option sets the UI may choose from.
//
// Nothing here accepts raw ffmpeg/yt-dlp/ImageMagick flags. Every value a
// client sends is matched against a fixed table before a runner turns it into
// argv elements, so a malicious payload cannot smuggle in extra options.
package presets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var (
	VideoContainers = []Option{
		{"mp4", "MP4"}, {"mkv", "MKV"}, {"webm", "WebM"}, {"mov", "MOV"},
	}
	VideoCodecs = []Option{
		{"h264", "H.264 (widest support)"},
		{"h265", "H.265 / HEVC (smaller)"},
		{"vp9", "VP9"},
		{"av1", "AV1 (slowest, smallest)"},
		{"copy", "Copy (no re-encode)"},
	}
	// Values are ffmpeg -preset names; the labels name the trade instead.
	EncodeSpeeds = []Option{
		{"veryfast", "Quickest - biggest file"},
		{"faster", "Quicker - bigger file"},
		{"fast", "Quick"},
		{"medium", "Normal (recommended)"},
		{"slow", "Slower - smaller file"},
		{"veryslow", "Slowest - smallest file"},
	}
	Resolutions = []Option{
		{"source", "Source"}, {"2160", "4K (2160p)"}, {"1440", "1440p"},
		{"1080", "1080p"}, {"720", "720p"}, {"480", "480p"}, {"360", "360p"},
	}
	FrameRates = []Option{
		{"source", "Source"}, {"60", "60 fps"}, {"30", "30 fps"}, {"24", "24 fps"},
	}
	AudioCodecs = []Option{
		{"aac", "AAC"}, {"opus", "Opus"}, {"mp3", "MP3"}, {"flac", "FLAC (lossless)"},
		{"copy", "Copy"}, {"none", "Remove audio"},
	}
	AudioBitrates = []Option{
		{"64", "64 kbps"}, {"96", "96 kbps"}, {"128", "128 kbps"},
		{"192", "192 kbps"}, {"256", "256 kbps"}, {"320", "320 kbps"},
	}
	AudioFormats = []Option{
		{"mp3", "MP3"}, {"m4a", "M4A / AAC"}, {"opus", "Opus"},
		{"flac", "FLAC"}, {"wav", "WAV"},
	}
	ImageFormats = []Option{
		{"source", "Same as the input"},
		{"jpg", "JPEG"}, {"png", "PNG"}, {"webp", "WebP"},
		{"avif", "AVIF"}, {"gif", "GIF"}, {"tiff", "TIFF"},
		{"svg", "SVG (embeds the picture)"},
		{"svg_trace", "SVG (traced outlines, black and white)"},
	}
	EncodePresets = []Option{
		{"quality", "Best quality - bigger file, slower"},
		{"balanced", "Balanced - good quality, reasonable size"},
		{"speed", "Fastest encode - larger file"},
		{"size", "Smallest file - slower"},
		{"custom", "Custom - set everything myself"},
	}
	CompressTargets = []Option{
		{"light", "Light - near-original quality"},
		{"balanced", "Balanced - good quality, much smaller"},
		{"aggressive", "Aggressive - smallest file"},
		{"target_size", "Target file size - I'll pick the megabytes"},
	}
	ImageQualityPresets = []Option{
		{"original", "Original - no re-encode"},
		{"high", "High - near-original quality"},
		{"balanced", "Balanced - good quality, smaller file"},
		{"small", "Small - smaller file, visible loss"},
		{"custom", "Custom - set the quality myself"},
	}
	// Which side of the threshold potrace turns into paths.
	TraceTargets = []Option{
		{"dark", "The dark areas"},
		{"light", "The light areas"},
	}
	RetentionChoices = []Option{
		{"1", "1 day"}, {"7", "7 days"}, {"30", "30 days"}, {"0", "Forever"},
	}
)

// Accepted quality range per video codec; a lower CRF is better.
var CRFBounds = map[string][2]int{
	"h264": {14, 34},
	"h265": {18, 38},
	"vp9":  {20, 45},
	"av1":  {20, 50},
}

var CompressCRF = map[string]map[string]int{
	"light":      {"h264": 20, "h265": 24, "vp9": 28, "av1": 30},
	"balanced":   {"h264": 24, "h265": 28, "vp9": 33, "av1": 36},
	"aggressive": {"h264": 30, "h265": 34, "vp9": 40, "av1": 44},
}

// What a compress target fills into the form below it. Suggestions only: the
// client writes them into the visible fields and nothing here is enforced.
var CompressTargetTune = map[string]struct {
	Speed        string
	Resolution   string
	AudioBitrate string
}{
	"light":       {"fast", "source", "192"},
	"balanced":    {"medium", "source", "128"},
	"aggressive":  {"slow", "720", "96"},
	"target_size": {"medium", "source", "128"},
}

// "custom" is deliberately absent: it leaves the client's own choice alone.
var EncodePresetSpeed = map[string]string{
	"quality":  "slow",
	"balanced": "medium",
	"speed":    "veryfast",
	"size":     "slow", // a small file is worth a slow encode
}

// Per-codec, not shared: each encoder has its own quality scale, so every
// entry sits inside that codec's CRFBounds.
var EncodePresetCRF = map[string]map[string]int{
	"quality":  {"h264": 18, "h265": 22, "vp9": 24, "av1": 26},
	"balanced": {"h264": 23, "h265": 27, "vp9": 31, "av1": 34},
	"speed":    {"h264": 25, "h265": 29, "vp9": 33, "av1": 36},
	"size":     {"h264": 30, "h265": 34, "vp9": 40, "av1": 45},
}

// CompressCRF's still-image counterpart. "target_size" is absent: hitting an
// exact byte count would need an iterative quality search, which we do not do.
var ImageCompressQuality = map[string]int{
	"light":      90,
	"balanced":   78,
	"aggressive": 62,
}

// What each convert-tab quality preset resolves to. "original" is the 0
// sentinel that skips -quality entirely (see magickArgs).
var ImageConvertQuality = map[string]int{
	"original": 0,
	"high":     92,
	"balanced": 80,
	"small":    60,
}

const imageSourceFormat = "source"

// Only formats this tool can also write appear here, so "source" resolves to a
// name from this table rather than from the filename, and never to something
// we would ask ImageMagick to encode blind.
var imageFormatByExt = map[string]string{
	"jpg":  "jpg",
	"jpeg": "jpg",
	"png":  "png",
	"webp": "webp",
	"avif": "avif",
	"gif":  "gif",
	"tif":  "tiff",
	"tiff": "tiff",
	"svg":  "svg",
}

// Size target bounds, in megabytes.
const (
	TargetSizeMinMB = 1
	TargetSizeMaxMB = 65536
)

// targetSize switches a compress job from a quality preset to a bitrate budget.
const targetSize = "target_size"

type DownloadParams struct {
	Mode          string `json:"mode"`          // "video" | "audio"
	FormatID      string `json:"format_id"`     // yt-dlp format selector, pattern-checked
	AudioFormat   string `json:"audio_format"`  // when Mode == "audio"
	AudioBitrate  string `json:"audio_bitrate"` // when Mode == "audio"
	Container     string `json:"container"`     // optional remux target for video
	EmbedSubs     bool   `json:"embed_subs"`
	EmbedThumb    bool   `json:"embed_thumbnail"`
	EmbedMetadata bool   `json:"embed_metadata"`
	RetentionDays int    `json:"retention_days"` // 0 = forever
}

type ConvertParams struct {
	Preset        string `json:"preset"` // intent-level; "custom" hands control back
	Container     string `json:"container"`
	VideoCodec    string `json:"video_codec"`
	Speed         string `json:"speed"`
	CRF           int    `json:"crf"`
	Resolution    string `json:"resolution"`
	FrameRate     string `json:"frame_rate"`
	AudioCodec    string `json:"audio_codec"`
	AudioBitrate  string `json:"audio_bitrate"`
	StripMetadata bool   `json:"strip_metadata"`
	RetentionDays int    `json:"retention_days"`
}

type CompressParams struct {
	Target        string `json:"target"`
	TargetSizeMB  int    `json:"target_size_mb"` // only when Target == "target_size"
	Container     string `json:"container"`
	VideoCodec    string `json:"video_codec"`
	Speed         string `json:"speed"`
	Resolution    string `json:"resolution"`
	AudioBitrate  string `json:"audio_bitrate"`
	StripMetadata bool   `json:"strip_metadata"`
	RetentionDays int    `json:"retention_days"`
}

type ImageParams struct {
	Format        string `json:"format"`
	Trace         string `json:"trace"`   // svg_trace only
	Quality       int    `json:"quality"` // 0..100, 0 keeps the encoder's default ("original")
	Width         int    `json:"width"`   // 0 = keep
	Height        int    `json:"height"`  // 0 = keep
	StripMetadata bool   `json:"strip_metadata"`
	RetentionDays int    `json:"retention_days"`
}

type EditParams struct {
	Start         float64 `json:"start"`  // seconds into the source
	End           float64 `json:"end"`    // seconds, 0 keeps everything after Start
	CropX         int     `json:"crop_x"` // pixels, source coordinates
	CropY         int     `json:"crop_y"`
	CropW         int     `json:"crop_w"` // 0 = keep the whole frame
	CropH         int     `json:"crop_h"`
	RetentionDays int     `json:"retention_days"`
}

// ValidationError names the offending field so the UI can highlight it.
type ValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, reason string) error { return &ValidationError{Field: field, Reason: reason} }

// Plain ids, the bestvideo/bestaudio keywords and "+" merges only: selector
// syntax such as "best[height<=1080]" must never reach yt-dlp. Each alternative
// must open with an alphanumeric so no part of the value can look like a flag,
// keeping the package invariant that client text never becomes an option.
var formatIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]*(\+[A-Za-z0-9][A-Za-z0-9_.\-]*)?$`)

func ParseDownload(raw json.RawMessage, defaultRetention int) (*DownloadParams, error) {
	p := &DownloadParams{Mode: "video", RetentionDays: defaultRetention}
	if err := decode(raw, p); err != nil {
		return nil, err
	}
	if p.Mode != "video" && p.Mode != "audio" {
		return nil, invalid("mode", "must be video or audio")
	}
	if p.FormatID != "" && !formatIDRe.MatchString(p.FormatID) {
		return nil, invalid("format_id", "contains disallowed characters")
	}
	if len(p.FormatID) > 64 {
		return nil, invalid("format_id", "too long")
	}
	if p.Mode == "audio" {
		if err := oneOf("audio_format", p.AudioFormat, AudioFormats); err != nil {
			return nil, err
		}
		if p.AudioBitrate != "" {
			if err := oneOf("audio_bitrate", p.AudioBitrate, AudioBitrates); err != nil {
				return nil, err
			}
		}
	} else if p.Container != "" {
		if err := oneOf("container", p.Container, VideoContainers); err != nil {
			return nil, err
		}
	}
	if err := checkRetention(p.RetentionDays); err != nil {
		return nil, err
	}
	return p, nil
}

func ParseConvert(raw json.RawMessage, defaultRetention int) (*ConvertParams, error) {
	p := &ConvertParams{
		Preset: "custom", Container: "mp4", VideoCodec: "h264", Speed: "medium", CRF: 23,
		Resolution: "source", FrameRate: "source", AudioCodec: "aac",
		AudioBitrate: "192", RetentionDays: defaultRetention,
	}
	if err := decode(raw, p); err != nil {
		return nil, err
	}
	if err := oneOf("container", p.Container, VideoContainers); err != nil {
		return nil, err
	}
	if err := oneOf("video_codec", p.VideoCodec, VideoCodecs); err != nil {
		return nil, err
	}
	if err := oneOf("preset", p.Preset, EncodePresets); err != nil {
		return nil, err
	}
	if err := p.checkPreset(); err != nil {
		return nil, err
	}
	if err := oneOf("resolution", p.Resolution, Resolutions); err != nil {
		return nil, err
	}
	if err := oneOf("frame_rate", p.FrameRate, FrameRates); err != nil {
		return nil, err
	}
	if err := oneOf("audio_codec", p.AudioCodec, AudioCodecs); err != nil {
		return nil, err
	}
	if p.AudioCodec != "none" && p.AudioCodec != "copy" && p.AudioCodec != "flac" {
		if err := oneOf("audio_bitrate", p.AudioBitrate, AudioBitrates); err != nil {
			return nil, err
		}
	}
	if err := checkContainerCodec(p.Container, p.VideoCodec, p.AudioCodec); err != nil {
		return nil, err
	}
	if err := checkRetention(p.RetentionDays); err != nil {
		return nil, err
	}
	// Fold the preset in so the job record shows what the runner will execute.
	p.Speed, p.CRF = p.Resolved()
	return p, nil
}

// The raw speed/crf knobs are only checked for "custom": any other preset
// overrides both, so rejecting a client that never sent them would be wrong.
func (p *ConvertParams) checkPreset() error {
	if p.Preset != "custom" {
		if p.VideoCodec == "copy" {
			return invalid("preset",
				"there is nothing to tune when copying the video stream - choose the custom preset")
		}
		if _, ok := EncodePresetCRF[p.Preset][p.VideoCodec]; !ok {
			return invalid("preset", fmt.Sprintf("has no settings for the %s codec", p.VideoCodec))
		}
		return nil
	}
	if err := oneOf("speed", p.Speed, EncodeSpeeds); err != nil {
		return err
	}
	if p.VideoCodec == "copy" {
		return nil
	}
	b, ok := CRFBounds[p.VideoCodec]
	if !ok {
		return invalid("video_codec", "no quality range defined")
	}
	if p.CRF < b[0] || p.CRF > b[1] {
		return invalid("crf", fmt.Sprintf("must be between %d and %d for %s", b[0], b[1], p.VideoCodec))
	}
	return nil
}

// Resolved returns the speed and CRF the job should use: a preset wins over
// whatever the client sent, "custom" keeps it.
func (p *ConvertParams) Resolved() (speed string, crf int) {
	crf, ok := EncodePresetCRF[p.Preset][p.VideoCodec]
	if !ok {
		return p.Speed, p.CRF
	}
	return EncodePresetSpeed[p.Preset], crf
}

func ParseCompress(raw json.RawMessage, defaultRetention int) (*CompressParams, error) {
	p := &CompressParams{
		Target: "balanced", Container: "mp4", VideoCodec: "h264", Speed: "medium",
		Resolution: "source", AudioBitrate: "128", RetentionDays: defaultRetention,
	}
	if err := decode(raw, p); err != nil {
		return nil, err
	}
	if err := oneOf("target", p.Target, CompressTargets); err != nil {
		return nil, err
	}
	if err := oneOf("container", p.Container, VideoContainers); err != nil {
		return nil, err
	}
	if err := oneOf("speed", p.Speed, EncodeSpeeds); err != nil {
		return nil, err
	}
	if err := oneOf("resolution", p.Resolution, Resolutions); err != nil {
		return nil, err
	}
	if err := oneOf("audio_bitrate", p.AudioBitrate, AudioBitrates); err != nil {
		return nil, err
	}
	if err := p.checkTargetSize(); err != nil {
		return nil, err
	}
	if err := checkContainerCodec(p.Container, p.VideoCodec, DefaultAudioCodec(p.Container)); err != nil {
		return nil, err
	}
	if err := checkRetention(p.RetentionDays); err != nil {
		return nil, err
	}
	return p, nil
}

func ParseImage(raw json.RawMessage, defaultRetention int) (*ImageParams, error) {
	p := &ImageParams{Format: "webp", Trace: "dark", Quality: 82, StripMetadata: true, RetentionDays: defaultRetention}
	if err := decode(raw, p); err != nil {
		return nil, err
	}
	if err := oneOf("format", p.Format, ImageFormats); err != nil {
		return nil, err
	}
	if err := oneOf("trace", p.Trace, TraceTargets); err != nil {
		return nil, err
	}
	// 0 means "original": skip -quality and let the encoder use its own default.
	if p.Quality < 0 || p.Quality > 100 {
		return nil, invalid("quality", "must be between 0 and 100")
	}
	const maxDim = 20000
	if p.Width < 0 || p.Width > maxDim {
		return nil, invalid("width", fmt.Sprintf("must be between 0 and %d", maxDim))
	}
	if p.Height < 0 || p.Height > maxDim {
		return nil, invalid("height", fmt.Sprintf("must be between 0 and %d", maxDim))
	}
	if err := checkRetention(p.RetentionDays); err != nil {
		return nil, err
	}
	return p, nil
}

// Bounds for a trim: a cut shorter than this is a mis-tap, and no source this
// tool handles runs for a day.
const (
	editMaxSeconds = 24 * 3600
	editMinSpan    = 0.1
	cropMinPixels  = 16
	cropMaxPixels  = 20000
)

func ParseEdit(raw json.RawMessage, defaultRetention int) (*EditParams, error) {
	p := &EditParams{RetentionDays: defaultRetention}
	if err := decode(raw, p); err != nil {
		return nil, err
	}
	if p.Start < 0 || p.Start > editMaxSeconds {
		return nil, invalid("start", "is outside the file")
	}
	if p.End < 0 || p.End > editMaxSeconds {
		return nil, invalid("end", "is outside the file")
	}
	if p.End > 0 && p.End-p.Start < editMinSpan {
		return nil, invalid("end", "must be at least a tenth of a second after the start")
	}
	if err := p.checkCrop(); err != nil {
		return nil, err
	}
	if err := checkRetention(p.RetentionDays); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *EditParams) checkCrop() error {
	if p.CropW == 0 && p.CropH == 0 {
		if p.CropX != 0 || p.CropY != 0 {
			return invalid("crop_w", "an offset needs a crop size as well")
		}
		return nil
	}
	// H.264 cannot encode an odd width or height, so a stray pixel is dropped.
	p.CropW &^= 1
	p.CropH &^= 1
	for field, v := range map[string]int{"crop_w": p.CropW, "crop_h": p.CropH} {
		if v < cropMinPixels || v > cropMaxPixels {
			return invalid(field, fmt.Sprintf("must be between %d and %d pixels", cropMinPixels, cropMaxPixels))
		}
	}
	if p.CropX < 0 || p.CropY < 0 || p.CropX > cropMaxPixels || p.CropY > cropMaxPixels {
		return invalid("crop_x", "is outside the frame")
	}
	return nil
}

func (p *EditParams) HasCrop() bool { return p.CropW > 0 && p.CropH > 0 }

// Span is the output length in seconds, 0 when the cut runs to the end.
func (p *EditParams) Span() float64 {
	if p.End <= p.Start {
		return 0
	}
	return p.End - p.Start
}

// ResolveFormat needs the input path because "source" is only decided once
// there is a file: the extension picks an entry from imageFormatByExt, and a
// type we cannot write back out is refused here rather than at ImageMagick.
func (p *ImageParams) ResolveFormat(inputPath string) (string, error) {
	if p.Format != imageSourceFormat {
		return p.Format, nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(inputPath), "."))
	if ext == "" {
		return "", invalid("format",
			"this file has no extension, so its format cannot be kept - choose an output format")
	}
	f, ok := imageFormatByExt[ext]
	if !ok {
		return "", invalid("format",
			fmt.Sprintf("%s files cannot be written back as %s - choose an output format", strings.ToUpper(ext), strings.ToUpper(ext)))
	}
	return f, nil
}

// A quality target and a size budget are mutually exclusive: the first has no
// use for a megabyte count, the second no use for a CRF.
func (p *CompressParams) checkTargetSize() error {
	if !p.IsSizeTarget() {
		if p.TargetSizeMB != 0 {
			return invalid("target_size_mb", "is only accepted when target is target_size")
		}
		if _, ok := CompressCRF[p.Target][p.VideoCodec]; !ok {
			return invalid("video_codec", "not supported for compression")
		}
		return nil
	}
	if p.VideoCodec == "copy" {
		return invalid("video_codec",
			"a copied stream keeps its original bitrate, so it cannot hit a size target - pick a codec to re-encode with")
	}
	if _, ok := CRFBounds[p.VideoCodec]; !ok {
		return invalid("video_codec", "not supported for compression")
	}
	if p.TargetSizeMB == 0 {
		return invalid("target_size_mb", "is required when target is target_size")
	}
	if p.TargetSizeMB < TargetSizeMinMB || p.TargetSizeMB > TargetSizeMaxMB {
		return invalid("target_size_mb",
			fmt.Sprintf("must be between %d and %d MB", TargetSizeMinMB, TargetSizeMaxMB))
	}
	return nil
}

func (p *CompressParams) IsSizeTarget() bool { return p.Target == targetSize }

// CompressCRFFor is zero for a size target, which uses a bitrate budget instead.
func (p *CompressParams) CompressCRFFor() int { return CompressCRF[p.Target][p.VideoCodec] }

func decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &ValidationError{Field: "params", Reason: err.Error()}
	}
	return nil
}

func oneOf(field, value string, opts []Option) error {
	for _, o := range opts {
		if o.Value == value {
			return nil
		}
	}
	return invalid(field, fmt.Sprintf("%q is not an allowed value", value))
}

func checkRetention(days int) error {
	switch days {
	case 0, 1, 7, 30:
		return nil
	}
	return invalid("retention_days", "must be one of 0, 1, 7, 30")
}

// DefaultAudioCodec picks the audio codec for a job that does not offer the
// choice (compress): the container already decides it, and WebM only takes Opus.
// Both the validator and the runner read it, so they cannot disagree.
func DefaultAudioCodec(container string) string {
	if container == "webm" {
		return "opus"
	}
	return "aac"
}

// Rejects combinations ffmpeg would refuse to mux, before a job burns CPU.
func checkContainerCodec(container, vcodec, acodec string) error {
	if vcodec == "copy" || acodec == "copy" {
		return nil // the source decides; ffmpeg will error if it truly cannot mux
	}
	switch container {
	case "webm":
		if vcodec != "vp9" && vcodec != "av1" {
			return invalid("container", "WebM accepts only VP9 or AV1 video")
		}
		if acodec != "opus" && acodec != "none" {
			return invalid("audio_codec", "WebM accepts only Opus audio")
		}
	case "mp4", "mov":
		if vcodec == "vp9" {
			return invalid("container", "VP9 is not supported in MP4/MOV here - use WebM or MKV")
		}
		if acodec == "flac" {
			return invalid("audio_codec", "FLAC is not supported in MP4/MOV - use MKV")
		}
	}
	return nil
}

// Address ranges a download source may never point at. net/netip's own
// predicates cover the rest; these are the ones it considers ordinary unicast.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, incl. cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918, incl. the Docker bridge
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. 255.255.255.255
	netip.MustParsePrefix("::/96"),          // deprecated IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64, a route back to IPv4
	netip.MustParsePrefix("fc00::/7"),       // unique local
}

// How long a hostname lookup may take when the caller has no deadline of its own.
const sourceDNSTimeout = 5 * time.Second

// Swapped out in tests so validation never touches the network.
var lookupAddrs = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// SetSourceResolverForTests replaces the lookup URL validation performs and
// returns a function restoring the default. Tests in other packages need it to
// keep off the network now that validating a URL resolves its hostname;
// nothing outside a test should call it.
func SetSourceResolverForTests(fn func(ctx context.Context, host string) ([]netip.Addr, error)) (restore func()) {
	prev := lookupAddrs
	lookupAddrs = fn
	return func() { lookupAddrs = prev }
}

// ValidateSourceURL enforces an http/https allow-list before a URL reaches
// yt-dlp. Prefer ValidateSourceURLContext: this form resolves DNS under its own
// fixed timeout and cannot be cancelled by the caller.
func ValidateSourceURL(raw string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sourceDNSTimeout)
	defer cancel()
	return ValidateSourceURLContext(ctx, raw)
}

// ValidateSourceURLContext additionally requires the target to resolve to a
// public unicast address, so a download cannot be aimed at loopback, the LAN,
// or a cloud metadata service.
//
// Best-effort only: yt-dlp resolves the name again and follows redirects, so
// DNS rebinding or a 302 to an internal address still gets through. Egress
// filtering at the network layer is the control that actually holds.
func ValidateSourceURLContext(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalid("url", "is required")
	}
	if len(raw) > 2048 {
		return "", invalid("url", "is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid("url", "is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", invalid("url", "must start with http:// or https://")
	}
	if u.Host == "" {
		return "", invalid("url", "is missing a hostname")
	}
	// A leading dash would be read as a flag by yt-dlp's argument parser.
	if strings.HasPrefix(raw, "-") {
		return "", invalid("url", "must not start with a dash")
	}
	if err := checkSourceHost(ctx, u.Hostname()); err != nil {
		return "", err
	}
	return u.String(), nil
}

// The message deliberately names no address: the caller learns that the target
// is off-limits, not what the resolver said.
const notPublicReason = "must point at a public internet address - loopback, link-local, " +
	"multicast and private or LAN addresses are refused"

func checkSourceHost(ctx context.Context, host string) error {
	if host == "" {
		return invalid("url", "is missing a hostname")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !addrPermitted(addr) {
			return invalid("url", notPublicReason)
		}
		return nil
	}
	if !isDNSName(host) {
		return invalid("url", "hostname is not valid")
	}
	addrs, err := lookupAddrs(ctx, host)
	if err != nil || len(addrs) == 0 {
		return invalid("url", "hostname could not be resolved")
	}
	// Every record has to pass: a name that answers with one public and one
	// internal address must not be usable to reach the internal one.
	for _, addr := range addrs {
		if !addrPermitted(addr) {
			return invalid("url", notPublicReason)
		}
	}
	return nil
}

func addrPermitted(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	// Unmap so ::ffff:127.0.0.1 is judged as 127.0.0.1; drop the zone so the
	// prefix table can match a scoped address too.
	addr = addr.Unmap().WithZone("")
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Legacy numeric hosts ("2130706433", "0177.0.0.1", "0x7f000001") are read back
// as addresses by yt-dlp's HTTP stack but not by netip, so they would skip the
// address checks entirely. A real hostname's last label always starts with a
// letter, which none of those forms do.
func isDNSName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			default:
				return false
			}
		}
	}
	last := host[strings.LastIndexByte(host, '.')+1:]
	c := last[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// All lets the UI render its pickers without duplicating the allow-list in JavaScript.
func All() map[string]any {
	return map[string]any{
		"video_containers":  VideoContainers,
		"video_codecs":      VideoCodecs,
		"encode_speeds":     EncodeSpeeds,
		"resolutions":       Resolutions,
		"frame_rates":       FrameRates,
		"audio_codecs":      AudioCodecs,
		"audio_bitrates":    AudioBitrates,
		"encode_presets":    EncodePresets,
		"audio_formats":     AudioFormats,
		"image_formats":     ImageFormats,
		"compress_targets":  CompressTargets,
		"retention_choices": RetentionChoices,
		"trace_targets":     TraceTargets,
		"crf_bounds":        CRFBounds,
		"target_size_bounds": map[string]int{
			"min_mb": TargetSizeMinMB,
			"max_mb": TargetSizeMaxMB,
		},
		"encode_preset_values":   encodePresetValues(),
		"compress_target_values": compressTargetValues(),
		"image_compress_quality": ImageCompressQuality,
		"image_quality_presets":  ImageQualityPresets,
		"image_convert_quality":  ImageConvertQuality,
	}
}

// EncodePresetValue publishes what a preset resolves to, rather than hiding
// speed and CRF behind a label the user cannot see through.
type EncodePresetValue struct {
	Speed string         `json:"speed"`
	CRF   map[string]int `json:"crf"`
}

// Derived from the tables Resolved reads, so the two cannot drift apart.
func encodePresetValues() map[string]EncodePresetValue {
	out := make(map[string]EncodePresetValue, len(EncodePresetSpeed))
	for preset, speed := range EncodePresetSpeed {
		out[preset] = EncodePresetValue{Speed: speed, CRF: maps.Clone(EncodePresetCRF[preset])}
	}
	return out
}

// CRF is what the server uses; the rest are only what the form is filled with.
type CompressTargetValue struct {
	CRF          map[string]int `json:"crf"`
	Speed        string         `json:"speed"`
	Resolution   string         `json:"resolution"`
	AudioBitrate string         `json:"audio_bitrate"`
}

// Derived from the maps the server and client read, so they cannot drift.
func compressTargetValues() map[string]CompressTargetValue {
	out := make(map[string]CompressTargetValue, len(CompressTargetTune))
	for target, tune := range CompressTargetTune {
		out[target] = CompressTargetValue{
			CRF:          maps.Clone(CompressCRF[target]),
			Speed:        tune.Speed,
			Resolution:   tune.Resolution,
			AudioBitrate: tune.AudioBitrate,
		}
	}
	return out
}
