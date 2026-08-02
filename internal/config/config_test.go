package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Every variable Load reads is cleared first, so an operator's own environment
// cannot change the result, then DATA_DIR is pointed at a scratch directory
// because Load creates the tree it describes.
func withEnv(t *testing.T, env map[string]string) string {
	t.Helper()
	for _, k := range []string{
		"LISTEN_ADDR", "DATA_DIR", "DB_PATH", "DOWNLOAD_DIR", "TMP_DIR", "UPLOAD_DIR",
		"MAX_UPLOAD_MIB", "MAX_CONCURRENT_JOBS", "QUEUE_DEPTH", "DEFAULT_RETENTION_DAYS",
		"AUTH_USERNAME", "AUTH_PASSWORD_HASH", "SESSION_TTL_HOURS", "XACCEL_PREFIX",
		"LOG_LEVEL", "YTDLP_BIN", "FFMPEG_BIN", "FFPROBE_BIN", "MAGICK_BIN",
		"TRUSTED_PROXIES", "ALLOWED_HOSTS", "PROXY_ONLY",
	} {
		// t.Setenv first so the original value is restored on cleanup; the
		// unset matters because an explicitly empty XACCEL_PREFIX is not the
		// same thing as an absent one.
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	for k, v := range env {
		t.Setenv(k, v)
	}
	return dir
}

func TestLoadDefaults(t *testing.T) {
	dir := withEnv(t, nil)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Addr != "127.0.0.1:1144" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.MaxConcurrentJobs != 2 || c.QueueDepth != 256 || c.DefaultRetentionDays != 7 {
		t.Errorf("unexpected job defaults: %+v", c)
	}
	if c.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want warn", c.LogLevel)
	}
	if c.AuthEnabled() {
		t.Error("auth should be disabled when no credentials are set")
	}
	if c.MaxUploadBytes() != 4096<<20 {
		t.Errorf("MaxUploadBytes = %d", c.MaxUploadBytes())
	}
	if c.XAccelPrefix != "/_protected" {
		t.Errorf("XAccelPrefix = %q", c.XAccelPrefix)
	}
	if c.DBPath != filepath.Join(dir, "db", "aiofiles.db") {
		t.Errorf("DBPath = %q", c.DBPath)
	}
	for _, d := range []string{filepath.Dir(c.DBPath), c.DownloadDir, c.TmpDir, c.UploadDir} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Errorf("Load did not create %s: %v", d, err)
		}
	}
}

// Half-configured auth is the mistake that silently leaves an instance open.
func TestLoadAuthIsBothOrNeither(t *testing.T) {
	const hash = "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2g"

	tests := []struct {
		name        string
		env         map[string]string
		wantErr     bool
		wantEnabled bool
	}{
		{"neither", nil, false, false},
		{"username only", map[string]string{"AUTH_USERNAME": "admin"}, true, false},
		{"hash only", map[string]string{"AUTH_PASSWORD_HASH": hash}, true, false},
		{"both", map[string]string{"AUTH_USERNAME": "admin", "AUTH_PASSWORD_HASH": hash}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load accepted a half-configured login")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.AuthEnabled() != tc.wantEnabled {
				t.Errorf("AuthEnabled = %v, want %v", c.AuthEnabled(), tc.wantEnabled)
			}
		})
	}
}

func TestLoadRetentionMenu(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"0", false}, {"1", false}, {"7", false}, {"30", false},
		{"2", true}, {"5", true}, {"14", true}, {"365", true}, {"-1", true},
	}
	for _, tc := range tests {
		t.Run("DEFAULT_RETENTION_DAYS="+tc.value, func(t *testing.T) {
			withEnv(t, map[string]string{"DEFAULT_RETENTION_DAYS": tc.value})
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load accepted retention %q, which presets.Parse* would reject on every job", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.DefaultRetentionDays; got < 0 {
				t.Errorf("DefaultRetentionDays = %d", got)
			}
		})
	}
}

func TestLoadConcurrencyAndLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, *Config)
	}{
		{"one worker", map[string]string{"MAX_CONCURRENT_JOBS": "1"}, false,
			func(t *testing.T, c *Config) {
				if c.MaxConcurrentJobs != 1 {
					t.Errorf("MaxConcurrentJobs = %d", c.MaxConcurrentJobs)
				}
			}},
		{"zero workers", map[string]string{"MAX_CONCURRENT_JOBS": "0"}, true, nil},
		{"negative workers", map[string]string{"MAX_CONCURRENT_JOBS": "-4"}, true, nil},
		// A typo falls back to the default rather than failing to boot.
		{"unparseable workers", map[string]string{"MAX_CONCURRENT_JOBS": "two"}, false,
			func(t *testing.T, c *Config) {
				if c.MaxConcurrentJobs != 2 {
					t.Errorf("MaxConcurrentJobs = %d, want the default 2", c.MaxConcurrentJobs)
				}
			}},
		{"log level debug", map[string]string{"LOG_LEVEL": "debug"}, false,
			func(t *testing.T, c *Config) {
				if c.LogLevel != slog.LevelDebug {
					t.Errorf("LogLevel = %v", c.LogLevel)
				}
			}},
		{"log level warning alias", map[string]string{"LOG_LEVEL": " WARNING "}, false,
			func(t *testing.T, c *Config) {
				if c.LogLevel != slog.LevelWarn {
					t.Errorf("LogLevel = %v", c.LogLevel)
				}
			}},
		{"unknown log level", map[string]string{"LOG_LEVEL": "verbose"}, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load accepted an invalid setting")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, c)
		})
	}
}

// 0 has to mean "unlimited" here, because that is what the upload handler and
// the rendered `client_max_body_size 0` both do with it.
func TestLoadMaxUpload(t *testing.T) {
	tests := []struct {
		value     string
		wantErr   bool
		wantBytes int64
	}{
		{"4096", false, 4096 << 20},
		{"0", false, 0},
		{"1", false, 1 << 20},
		{"-1", true, 0},
		{"-4096", true, 0},
		// Would overflow int64 in MaxUploadBytes and read back as unlimited.
		{"9999999999999999", true, 0},
		// Unparseable still falls back to the default, now with a warning.
		{"4G", false, 4096 << 20},
	}
	for _, tc := range tests {
		t.Run("MAX_UPLOAD_MIB="+tc.value, func(t *testing.T) {
			withEnv(t, map[string]string{"MAX_UPLOAD_MIB": tc.value})
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load accepted MAX_UPLOAD_MIB=%q", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.MaxUploadBytes() != tc.wantBytes {
				t.Errorf("MaxUploadBytes = %d, want %d", c.MaxUploadBytes(), tc.wantBytes)
			}
		})
	}
}

func TestLoadXAccelPrefix(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		want    string
	}{
		{"unset", nil, false, "/_protected"},
		// Explicitly empty is the documented way to make Go stream the file.
		{"empty", map[string]string{"XACCEL_PREFIX": ""}, false, ""},
		{"custom", map[string]string{"XACCEL_PREFIX": "/_dl"}, false, "/_dl"},
		{"padded", map[string]string{"XACCEL_PREFIX": "  /_dl  "}, false, "/_dl"},
		{"relative", map[string]string{"XACCEL_PREFIX": "_protected"}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load accepted a prefix that matches no nginx location")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.XAccelPrefix != tc.want {
				t.Errorf("XAccelPrefix = %q, want %q", c.XAccelPrefix, tc.want)
			}
		})
	}
}

func TestLoadPathOverrides(t *testing.T) {
	base := t.TempDir()
	withEnv(t, map[string]string{
		"DOWNLOAD_DIR":   filepath.Join(base, "out"),
		"UPLOAD_DIR":     filepath.Join(base, "in"),
		"MAX_UPLOAD_MIB": "10",
	})

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DownloadDir != filepath.Join(base, "out") || c.UploadDir != filepath.Join(base, "in") {
		t.Errorf("overrides ignored: %+v", c)
	}
	if c.MaxUploadBytes() != 10<<20 {
		t.Errorf("MaxUploadBytes = %d, want %d", c.MaxUploadBytes(), 10<<20)
	}
	for _, d := range []string{c.DownloadDir, c.UploadDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("Load did not create %s: %v", d, err)
		}
	}
}

// The shipped default has to stay "reachable by IP on a LAN", so an instance
// that sets nothing keeps answering to every name.
func TestLoadProxyDefaults(t *testing.T) {
	withEnv(t, nil)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TrustedProxies) != 0 || len(c.AllowedHosts) != 0 || c.ProxyOnly {
		t.Errorf("proxy settings are not off by default: %+v", c)
	}
	if !c.HostAllowed("192.168.16.35") || !c.HostAllowed("aio.example.com") {
		t.Error("an empty ALLOWED_HOSTS must allow everything")
	}
}

func TestLoadTrustedProxies(t *testing.T) {
	tests := []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"10.0.0.0/8", []string{"10.0.0.0/8"}, false},
		// A bare address stands for itself, and the list tolerates the spacing
		// a human writes in a .env file.
		{"10.0.0.5, 192.168.1.0/24 ,,2001:db8::1", []string{
			"10.0.0.5/32", "192.168.1.0/24", "2001:db8::1/128"}, false},
		// Host bits set is a typo worth normalising rather than rejecting.
		{"10.1.2.3/8", []string{"10.0.0.0/8"}, false},
		{"not-an-ip", nil, true},
		{"10.0.0.0/64", nil, true},
	}
	for _, tc := range tests {
		t.Run("TRUSTED_PROXIES="+tc.value, func(t *testing.T) {
			withEnv(t, map[string]string{"TRUSTED_PROXIES": tc.value})
			c, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load accepted an unparseable TRUSTED_PROXIES")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(c.TrustedProxies) != len(tc.want) {
				t.Fatalf("TrustedProxies = %v, want %v", c.TrustedProxies, tc.want)
			}
			for i, p := range c.TrustedProxies {
				if p.String() != tc.want[i] {
					t.Errorf("TrustedProxies[%d] = %s, want %s", i, p, tc.want[i])
				}
			}
		})
	}
}

func TestLoadAllowedHosts(t *testing.T) {
	withEnv(t, map[string]string{
		"ALLOWED_HOSTS": " AIO.example.com , *.media.example.com ,, 192.168.16.35 ",
	})

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	allowed := []string{"aio.example.com", "files.media.example.com", "a.b.media.example.com", "192.168.16.35"}
	for _, h := range allowed {
		if !c.HostAllowed(h) {
			t.Errorf("HostAllowed(%q) = false, want true", h)
		}
	}
	blocked := []string{
		"evil.example.com",
		"media.example.com", // the bare domain is not one of its subdomains
		"aio.example.com.evil.net",
		"xaio.example.com",
		"192.168.16.36",
		"",
	}
	for _, h := range blocked {
		if c.HostAllowed(h) {
			t.Errorf("HostAllowed(%q) = true, want false", h)
		}
	}
}

func TestLoadAllowedHostsRejectsNonsense(t *testing.T) {
	for _, v := range []string{
		"https://aio.example.com", // scheme
		"aio.example.com:8443",    // port
		"aio.example.com/app",     // path
		"aio.*.example.com",       // wildcard anywhere but in front
		"*.",
	} {
		t.Run(v, func(t *testing.T) {
			withEnv(t, map[string]string{"ALLOWED_HOSTS": v})
			if _, err := Load(); err == nil {
				t.Errorf("Load accepted ALLOWED_HOSTS=%q", v)
			}
		})
	}
}

// "*" is the documented escape hatch for someone who wants PROXY_ONLY without
// pinning a name.
func TestLoadAllowedHostsWildcard(t *testing.T) {
	withEnv(t, map[string]string{"ALLOWED_HOSTS": "*"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.HostAllowed("anything.example.com") || !c.HostAllowed("192.168.16.35") {
		t.Error("ALLOWED_HOSTS=* must allow everything")
	}
}

func TestLoadProxyOnly(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"YES", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false},
		{"", false},
		{"maybe", false}, // unparseable falls back to the default, with a warning
	}
	for _, tc := range tests {
		t.Run("PROXY_ONLY="+tc.value, func(t *testing.T) {
			withEnv(t, map[string]string{"PROXY_ONLY": tc.value})
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.ProxyOnly != tc.want {
				t.Errorf("ProxyOnly = %v, want %v", c.ProxyOnly, tc.want)
			}
		})
	}
}
