// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr     string // e.g. 127.0.0.1:1144
	LogLevel slog.Level

	DataDir      string
	DBPath       string
	DownloadDir  string
	TmpDir       string
	UploadDir    string
	MaxUploadMiB int64 // 0 means unlimited, in Go and in nginx

	MaxConcurrentJobs    int
	QueueDepth           int
	DefaultRetentionDays int
	JobTimeoutMinutes    int // 0 means no limit

	AuthUsername     string
	AuthPasswordHash string // argon2id encoded hash; empty means auth disabled
	SessionTTLHours  int

	XAccelPrefix string // internal nginx location; empty streams through Go instead

	YtDlpBin   string
	FFmpegBin  string
	FFprobeBin string
	MagickBin  string
	RsvgBin    string
	PotraceBin string
}

// Above this MaxUploadBytes overflows int64 and wraps to a negative number,
// which the upload handler would read as "unlimited".
const maxUploadMiBCeiling = (1<<63 - 1) >> 20

// Above this JobTimeout overflows time.Duration (int64 nanoseconds) and wraps
// to a negative deadline, which would kill every job the moment it starts.
const jobTimeoutMinutesCeiling = (1<<63 - 1) / int64(time.Minute)

func Load() (*Config, error) {
	c := &Config{
		Addr:                 env("LISTEN_ADDR", "127.0.0.1:1144"),
		DataDir:              env("DATA_DIR", "/data"),
		MaxUploadMiB:         int64(envInt("MAX_UPLOAD_MIB", 4096)),
		MaxConcurrentJobs:    envInt("MAX_CONCURRENT_JOBS", 2),
		QueueDepth:           envInt("QUEUE_DEPTH", 256),
		DefaultRetentionDays: envInt("DEFAULT_RETENTION_DAYS", 7),
		JobTimeoutMinutes:    envInt("JOB_TIMEOUT_MINUTES", 720),
		AuthUsername:         env("AUTH_USERNAME", ""),
		AuthPasswordHash:     env("AUTH_PASSWORD_HASH", ""),
		SessionTTLHours:      envInt("SESSION_TTL_HOURS", 720),
		YtDlpBin:             env("YTDLP_BIN", "yt-dlp"),
		FFmpegBin:            env("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:           env("FFPROBE_BIN", "ffprobe"),
		MagickBin:            env("MAGICK_BIN", "magick"),
		RsvgBin:              env("RSVG_BIN", "rsvg-convert"),
		PotraceBin:           env("POTRACE_BIN", "potrace"),
	}

	// Unlike every other string, an explicitly empty XACCEL_PREFIX is meaningful
	// (it turns the handoff off), so env() is not usable here.
	c.XAccelPrefix = "/_protected"
	if v, ok := os.LookupEnv("XACCEL_PREFIX"); ok {
		c.XAccelPrefix = strings.TrimSpace(v)
	}

	c.DBPath = env("DB_PATH", filepath.Join(c.DataDir, "db", "aiofiles.db"))
	c.DownloadDir = env("DOWNLOAD_DIR", filepath.Join(c.DataDir, "downloads"))
	c.TmpDir = env("TMP_DIR", filepath.Join(c.DataDir, "tmp"))
	c.UploadDir = env("UPLOAD_DIR", filepath.Join(c.DataDir, "uploads"))

	lvl, err := parseLevel(env("LOG_LEVEL", "warn"))
	if err != nil {
		return nil, err
	}
	c.LogLevel = lvl

	if c.MaxConcurrentJobs < 1 {
		return nil, fmt.Errorf("MAX_CONCURRENT_JOBS must be >= 1")
	}
	// 0 is unlimited on both sides: the upload handler skips its MaxBytesReader
	// and the init script renders `client_max_body_size 0`. The ceiling is the
	// point where MaxUploadBytes would overflow and silently become unlimited.
	if c.MaxUploadMiB < 0 || c.MaxUploadMiB > maxUploadMiBCeiling {
		return nil, fmt.Errorf("MAX_UPLOAD_MIB must be between 0 (unlimited) and %d (got %d)",
			maxUploadMiBCeiling, c.MaxUploadMiB)
	}
	// 0 is "no limit", spelled the same way as MAX_UPLOAD_MIB=0. Anything past
	// the ceiling would overflow the duration it is converted into.
	if c.JobTimeoutMinutes < 0 || int64(c.JobTimeoutMinutes) > jobTimeoutMinutesCeiling {
		return nil, fmt.Errorf("JOB_TIMEOUT_MINUTES must be between 0 (no limit) and %d (got %d)",
			jobTimeoutMinutesCeiling, c.JobTimeoutMinutes)
	}
	if c.XAccelPrefix != "" && !strings.HasPrefix(c.XAccelPrefix, "/") {
		return nil, fmt.Errorf("XACCEL_PREFIX must start with / or be empty (got %q)", c.XAccelPrefix)
	}
	// presets.Parse* enforces the same fixed menu; an out-of-menu default here
	// would fail validation on every job.
	switch c.DefaultRetentionDays {
	case 0, 1, 7, 30:
	default:
		return nil, fmt.Errorf("DEFAULT_RETENTION_DAYS must be one of 0, 1, 7, 30 (got %d)", c.DefaultRetentionDays)
	}
	if (c.AuthUsername == "") != (c.AuthPasswordHash == "") {
		return nil, fmt.Errorf("AUTH_USERNAME and AUTH_PASSWORD_HASH must both be set, or both empty")
	}

	for _, d := range []string{filepath.Dir(c.DBPath), c.DownloadDir, c.TmpDir, c.UploadDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return c, nil
}

func (c *Config) AuthEnabled() bool { return c.AuthUsername != "" && c.AuthPasswordHash != "" }

func (c *Config) MaxUploadBytes() int64 { return c.MaxUploadMiB << 20 }

// JobTimeout is the wall-clock budget for one job. Zero means no limit.
func (c *Config) JobTimeout() time.Duration {
	return time.Duration(c.JobTimeoutMinutes) * time.Minute
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		slog.Warn("ignoring unparseable value, using the default",
			"var", key, "value", v, "default", def)
		return def
	}
	return n
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown LOG_LEVEL %q", s)
}
