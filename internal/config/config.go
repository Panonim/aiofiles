// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
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

	// Reverse proxy. Loopback is always trusted on top of TrustedProxies,
	// because the bundled nginx is one; see internal/proxy.
	TrustedProxies []netip.Prefix // whose X-Forwarded-* headers are believed
	AllowedHosts   []string       // names the instance answers to; empty means any
	ProxyOnly      bool           // refuse requests that did not come through a proxy

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
		ProxyOnly:            envBool("PROXY_ONLY", false),
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

	if c.TrustedProxies, err = parsePrefixes(env("TRUSTED_PROXIES", "")); err != nil {
		return nil, err
	}
	if c.AllowedHosts, err = parseHosts(env("ALLOWED_HOSTS", "")); err != nil {
		return nil, err
	}

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

// HostAllowed matches a hostname - already lowercased and without its port -
// against ALLOWED_HOSTS. An empty list allows everything, which is the default:
// a LAN box reached by IP must keep working out of the box.
//
// The same matching is rendered into nginx's host map by the init script, so a
// name the app refuses is refused for the static frontend as well.
func (c *Config) HostAllowed(host string) bool {
	if len(c.AllowedHosts) == 0 {
		return true
	}
	for _, pat := range c.AllowedHosts {
		switch {
		case pat == "*":
			return true
		case strings.HasPrefix(pat, "*."):
			// "*.example.com" is subdomains only, at any depth. The bare domain
			// is a different name and has to be listed if it is wanted.
			if suffix := pat[1:]; len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
		case pat == host:
			return true
		}
	}
	return false
}

// parsePrefixes reads TRUSTED_PROXIES: a comma-separated list of CIDR blocks or
// bare addresses, the latter standing for themselves.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if p, err := netip.ParsePrefix(field); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %q is neither an IP address nor a CIDR block", field)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// parseHosts reads ALLOWED_HOSTS. Entries are hostnames, IP literals, "*.suffix"
// patterns, or a lone "*" meaning any. A port is rejected rather than ignored:
// the compared hostname never has one, so an entry carrying one would silently
// match nothing.
func parseHosts(s string) ([]string, error) {
	var out []string
	for _, field := range strings.Split(s, ",") {
		host := strings.ToLower(strings.TrimSpace(field))
		if host == "" {
			continue
		}
		if host == "*" {
			return []string{"*"}, nil
		}
		body := strings.TrimPrefix(host, "*.")
		if !isBracketedIPv6(body) && (body == "" || strings.ContainsAny(body, "*:/@[] ")) {
			return nil, fmt.Errorf("ALLOWED_HOSTS: %q must be a hostname, an IP literal, or *.suffix - no scheme, port or path", field)
		}
		out = append(out, host)
	}
	return out, nil
}

// An IPv6 literal in a Host header is bracketed, so it is the one entry that is
// allowed to contain colons.
func isBracketedIPv6(host string) bool {
	if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
		return false
	}
	addr, err := netip.ParseAddr(host[1 : len(host)-1])
	return err == nil && addr.Is6()
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

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	slog.Warn("ignoring unparseable value, using the default",
		"var", key, "value", v, "default", def)
	return def
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
