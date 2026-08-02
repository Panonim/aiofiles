package presets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// stubDNS points the package resolver at a fixed table for the duration of a
// test, so URL validation never touches the network.
func stubDNS(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := lookupAddrs
	t.Cleanup(func() { lookupAddrs = prev })
	lookupAddrs = func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		addrs := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, netip.MustParseAddr(ip))
		}
		return addrs, nil
	}
}

// wantField asserts err is a *ValidationError naming field.
func wantField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error for field %q, got nil", field)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Field != field {
		t.Fatalf("error names field %q (%s), want %q", ve.Field, ve.Reason, field)
	}
}

func TestParseDownloadAccepts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want DownloadParams
	}{
		{"empty body keeps defaults", ``, DownloadParams{Mode: "video", RetentionDays: 7}},
		{"explicit video", `{"mode":"video"}`, DownloadParams{Mode: "video", RetentionDays: 7}},
		{"plain format id", `{"format_id":"137"}`,
			DownloadParams{Mode: "video", FormatID: "137", RetentionDays: 7}},
		{"merge of two ids", `{"format_id":"137+140"}`,
			DownloadParams{Mode: "video", FormatID: "137+140", RetentionDays: 7}},
		{"keyword selector", `{"format_id":"bestvideo+bestaudio"}`,
			DownloadParams{Mode: "video", FormatID: "bestvideo+bestaudio", RetentionDays: 7}},
		// Real yt-dlp ids carry dashes, dots and underscores after the first character.
		{"dashed id", `{"format_id":"dash-video-1080"}`,
			DownloadParams{Mode: "video", FormatID: "dash-video-1080", RetentionDays: 7}},
		{"dotted merge", `{"format_id":"hls-1080.2+audio_1"}`,
			DownloadParams{Mode: "video", FormatID: "hls-1080.2+audio_1", RetentionDays: 7}},
		{"numeric merge", `{"format_id":"248+251"}`,
			DownloadParams{Mode: "video", FormatID: "248+251", RetentionDays: 7}},
		{"bestaudio alone", `{"format_id":"bestaudio"}`,
			DownloadParams{Mode: "video", FormatID: "bestaudio", RetentionDays: 7}},
		{"remux container", `{"container":"mkv"}`,
			DownloadParams{Mode: "video", Container: "mkv", RetentionDays: 7}},
		{"audio mode", `{"mode":"audio","audio_format":"mp3","audio_bitrate":"192"}`,
			DownloadParams{Mode: "audio", AudioFormat: "mp3", AudioBitrate: "192", RetentionDays: 7}},
		{"audio mode without bitrate", `{"mode":"audio","audio_format":"flac"}`,
			DownloadParams{Mode: "audio", AudioFormat: "flac", RetentionDays: 7}},
		{"forever retention", `{"retention_days":0}`, DownloadParams{Mode: "video"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDownload(json.RawMessage(tc.raw), 7)
			if err != nil {
				t.Fatalf("ParseDownload(%s) = %v", tc.raw, err)
			}
			if *got != tc.want {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func TestParseDownloadRejects(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		field string
	}{
		{"unknown mode", `{"mode":"torrent"}`, "mode"},
		// yt-dlp selector syntax must never reach the command line.
		{"height selector", `{"format_id":"best[height<=1080]"}`, "format_id"},
		{"slash fallback selector", `{"format_id":"bv*+ba/b"}`, "format_id"},
		{"space separated", `{"format_id":"137 140"}`, "format_id"},
		{"shell metacharacter", `{"format_id":"137;rm -rf /"}`, "format_id"},
		{"double merge", `{"format_id":"137+140+141"}`, "format_id"},
		{"quoted selector", `{"format_id":"'137'"}`, "format_id"},
		// Nothing that could be read as a flag if the value ever moved argv position.
		{"leading dash", `{"format_id":"--verbose"}`, "format_id"},
		{"single leading dash", `{"format_id":"-f"}`, "format_id"},
		{"leading dash after merge", `{"format_id":"137+-o"}`, "format_id"},
		{"leading dot", `{"format_id":".config"}`, "format_id"},
		{"leading underscore", `{"format_id":"_x"}`, "format_id"},
		{"over length id", `{"format_id":"` + strings.Repeat("a", 65) + `"}`, "format_id"},
		{"audio without format", `{"mode":"audio"}`, "audio_format"},
		{"unknown audio format", `{"mode":"audio","audio_format":"ogg"}`, "audio_format"},
		{"unknown audio bitrate", `{"mode":"audio","audio_format":"mp3","audio_bitrate":"999"}`, "audio_bitrate"},
		{"unknown container", `{"container":"avi"}`, "container"},
		{"retention off the menu", `{"retention_days":5}`, "retention_days"},
		{"unknown field", `{"proxy":"http://evil"}`, "params"},
		{"not an object", `[1,2,3]`, "params"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDownload(json.RawMessage(tc.raw), 7)
			wantField(t, err, tc.field)
		})
	}
}

func TestParseConvertAccepts(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantSpeed string
		wantCRF   int
	}{
		{"defaults", `{}`, "medium", 23},
		{"custom crf at lower bound", `{"crf":14}`, "medium", 14},
		{"custom crf at upper bound", `{"crf":34}`, "medium", 34},
		{"av1 has its own range", `{"video_codec":"av1","crf":50}`, "medium", 50},
		{"stream copy ignores crf", `{"video_codec":"copy","crf":0}`, "medium", 0},
		{"preset overrides speed and crf", `{"preset":"quality","speed":"veryfast","crf":34}`, "slow", 18},
		{"size preset", `{"preset":"size"}`, "slow", 30},
		{"webm with vp9 and opus", `{"container":"webm","video_codec":"vp9","audio_codec":"opus"}`, "medium", 23},
		{"audio removed", `{"audio_codec":"none"}`, "medium", 23},
		{"audio copied skips bitrate", `{"audio_codec":"copy","audio_bitrate":""}`, "medium", 23},
		{"flac in mkv", `{"container":"mkv","audio_codec":"flac"}`, "medium", 23},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConvert(json.RawMessage(tc.raw), 7)
			if err != nil {
				t.Fatalf("ParseConvert(%s) = %v", tc.raw, err)
			}
			if got.Speed != tc.wantSpeed || got.CRF != tc.wantCRF {
				t.Errorf("resolved speed/crf = %q/%d, want %q/%d",
					got.Speed, got.CRF, tc.wantSpeed, tc.wantCRF)
			}
		})
	}
}

func TestParseConvertRejects(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		field string
	}{
		{"unknown container", `{"container":"avi"}`, "container"},
		{"unknown codec", `{"video_codec":"divx"}`, "video_codec"},
		{"unknown preset", `{"preset":"turbo"}`, "preset"},
		{"unknown speed", `{"speed":"ludicrous"}`, "speed"},
		{"unknown resolution", `{"resolution":"4k"}`, "resolution"},
		{"unknown frame rate", `{"frame_rate":"120"}`, "frame_rate"},
		{"unknown audio codec", `{"audio_codec":"vorbis"}`, "audio_codec"},
		{"unknown audio bitrate", `{"audio_bitrate":"999"}`, "audio_bitrate"},
		{"crf below range", `{"crf":10}`, "crf"},
		{"crf above range", `{"crf":40}`, "crf"},
		{"crf outside codec range", `{"video_codec":"h265","crf":15}`, "crf"},
		{"negative crf", `{"crf":-1}`, "crf"},
		{"preset cannot tune a copy", `{"preset":"quality","video_codec":"copy"}`, "preset"},
		// Container/codec pairs ffmpeg could not mux.
		{"vp9 in mp4", `{"video_codec":"vp9"}`, "container"},
		{"vp9 in mov", `{"container":"mov","video_codec":"vp9"}`, "container"},
		{"h264 in webm", `{"container":"webm","video_codec":"h264"}`, "container"},
		{"aac in webm", `{"container":"webm","video_codec":"vp9"}`, "audio_codec"},
		{"flac in mp4", `{"audio_codec":"flac"}`, "audio_codec"},
		{"retention off the menu", `{"retention_days":365}`, "retention_days"},
		{"unknown field", `{"video_filter":"crop=1:1"}`, "params"},
		{"crf as string", `{"crf":"23"}`, "params"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConvert(json.RawMessage(tc.raw), 7)
			wantField(t, err, tc.field)
		})
	}
}

func TestParseCompress(t *testing.T) {
	t.Run("defaults resolve to a crf", func(t *testing.T) {
		got, err := ParseCompress(json.RawMessage(`{}`), 7)
		if err != nil {
			t.Fatalf("ParseCompress: %v", err)
		}
		if got.IsSizeTarget() {
			t.Error("default target should not be a size target")
		}
		if got.CompressCRFFor() != CompressCRF["balanced"]["h264"] {
			t.Errorf("crf = %d, want %d", got.CompressCRFFor(), CompressCRF["balanced"]["h264"])
		}
	})

	t.Run("size target", func(t *testing.T) {
		got, err := ParseCompress(json.RawMessage(`{"target":"target_size","target_size_mb":250}`), 7)
		if err != nil {
			t.Fatalf("ParseCompress: %v", err)
		}
		if !got.IsSizeTarget() || got.TargetSizeMB != 250 {
			t.Errorf("got %+v, want a 250 MB size target", got)
		}
	})

	// The container decides the audio codec, so WebM asks for Opus rather than
	// tripping checkContainerCodec with AAC.
	t.Run("webm with vp9", func(t *testing.T) {
		got, err := ParseCompress(json.RawMessage(`{"container":"webm","video_codec":"vp9"}`), 7)
		if err != nil {
			t.Fatalf("ParseCompress: %v", err)
		}
		if got.Container != "webm" || got.VideoCodec != "vp9" {
			t.Errorf("got %+v, want a webm/vp9 job", got)
		}
		if ac := DefaultAudioCodec(got.Container); ac != "opus" {
			t.Errorf("audio codec = %q, want opus", ac)
		}
	})

	t.Run("default audio codec muxes into every container", func(t *testing.T) {
		for _, c := range VideoContainers {
			for _, vc := range []string{"h264", "vp9", "av1"} {
				if err := checkContainerCodec(c.Value, vc, DefaultAudioCodec(c.Value)); err != nil {
					var ve *ValidationError
					if errors.As(err, &ve) && ve.Field == "audio_codec" {
						t.Errorf("%s/%s: default audio codec rejected: %v", c.Value, vc, err)
					}
				}
			}
		}
	})

	rejects := []struct {
		name  string
		raw   string
		field string
	}{
		{"unknown target", `{"target":"tiny"}`, "target"},
		{"unknown container", `{"container":"avi"}`, "container"},
		{"unknown speed", `{"speed":"ludicrous"}`, "speed"},
		{"unknown resolution", `{"resolution":"4k"}`, "resolution"},
		{"unknown audio bitrate", `{"audio_bitrate":"999"}`, "audio_bitrate"},
		{"size without megabytes", `{"target":"target_size"}`, "target_size_mb"},
		{"megabytes without size target", `{"target_size_mb":100}`, "target_size_mb"},
		{"megabytes below minimum", `{"target":"target_size","target_size_mb":-5}`, "target_size_mb"},
		{"megabytes above maximum", `{"target":"target_size","target_size_mb":70000}`, "target_size_mb"},
		{"copy cannot hit a size", `{"target":"target_size","target_size_mb":10,"video_codec":"copy"}`, "video_codec"},
		{"copy has no compression curve", `{"video_codec":"copy"}`, "video_codec"},
		{"unknown codec", `{"video_codec":"divx"}`, "video_codec"},
		{"vp9 in mp4", `{"video_codec":"vp9"}`, "container"},
		{"h264 in webm", `{"container":"webm","video_codec":"h264"}`, "container"},
		{"retention off the menu", `{"retention_days":2}`, "retention_days"},
		{"unknown field", `{"bitrate":"4000k"}`, "params"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCompress(json.RawMessage(tc.raw), 7)
			wantField(t, err, tc.field)
		})
	}
}

func TestParseImage(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := ParseImage(nil, 7)
		if err != nil {
			t.Fatalf("ParseImage: %v", err)
		}
		want := ImageParams{Format: "webp", Trace: "dark", Quality: 82, StripMetadata: true, RetentionDays: 7}
		if *got != want {
			t.Errorf("got %+v, want %+v", *got, want)
		}
	})

	rejects := []struct {
		name  string
		raw   string
		field string
	}{
		{"unknown format", `{"format":"bmp"}`, "format"},
		{"quality zero", `{"quality":0}`, "quality"},
		{"quality above 100", `{"quality":101}`, "quality"},
		{"negative quality", `{"quality":-1}`, "quality"},
		{"negative width", `{"width":-1}`, "width"},
		{"absurd width", `{"width":20001}`, "width"},
		{"negative height", `{"height":-1}`, "height"},
		{"absurd height", `{"height":20001}`, "height"},
		{"retention off the menu", `{"retention_days":90}`, "retention_days"},
		{"unknown field", `{"rotate":90}`, "params"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseImage(json.RawMessage(tc.raw), 7)
			wantField(t, err, tc.field)
		})
	}
}

func TestResolveFormat(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		input   string
		want    string
		wantErr bool
	}{
		{"explicit format ignores the input", "png", "/tmp/x.jpeg", "png", false},
		{"source from extension", "source", "/tmp/photo.jpeg", "jpg", false},
		{"source is case insensitive", "source", "/tmp/PHOTO.JPG", "jpg", false},
		{"tif normalises to tiff", "source", "/tmp/scan.TIF", "tiff", false},
		{"unwritable input format", "source", "/tmp/raw.cr2", "", true},
		{"no extension", "source", "/tmp/photo", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ImageParams{Format: tc.format}
			got, err := p.ResolveFormat(tc.input)
			if tc.wantErr {
				wantField(t, err, "format")
				return
			}
			if err != nil {
				t.Fatalf("ResolveFormat: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSourceURL(t *testing.T) {
	stubDNS(t, map[string][]string{
		"example.com":  {"93.184.216.34"},
		"example.com.": {"93.184.216.34"},
		"cdn.test":     {"2606:4700::6810:85e5"},
	})

	valid := []struct{ in, want string }{
		{"https://example.com/watch?v=abc", "https://example.com/watch?v=abc"},
		{"http://example.com/", "http://example.com/"},
		{"  https://example.com/x  ", "https://example.com/x"},
		{"https://cdn.test/a.mp4", "https://cdn.test/a.mp4"},
		{"https://example.com./x", "https://example.com./x"},
		// A literal public address is fine.
		{"https://93.184.216.34/x", "https://93.184.216.34/x"},
		{"https://[2606:4700::6810:85e5]/x", "https://[2606:4700::6810:85e5]/x"},
	}
	for _, tc := range valid {
		t.Run("accepts "+tc.in, func(t *testing.T) {
			got, err := ValidateSourceURL(tc.in)
			if err != nil {
				t.Fatalf("ValidateSourceURL(%q) = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	invalid := []struct{ name, in string }{
		{"empty", ""},
		{"blank", "   "},
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://example.com/x"},
		{"javascript scheme", "javascript:alert(1)"},
		{"no scheme", "example.com/x"},
		{"no host", "http://"},
		// A leading dash would be read as a flag by yt-dlp's argument parser.
		{"leading dash", "-x https://example.com"},
		{"leading dash only", "--exec=rm -rf /"},
		{"too long", "https://example.com/" + strings.Repeat("a", 2100)},
		{"unresolvable host", "https://nothing.invalid/x"},
	}
	for _, tc := range invalid {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := ValidateSourceURL(tc.in); err == nil {
				t.Fatalf("ValidateSourceURL(%q) accepted the input", tc.in)
			} else {
				wantField(t, err, "url")
			}
		})
	}
}

// Every blocked address class, reached both as a literal host and through a
// name, plus the IPv4-mapped IPv6 spellings an attacker would reach for.
func TestValidateSourceURLBlocksInternalAddresses(t *testing.T) {
	blocked := []struct{ name, addr string }{
		{"loopback v4", "127.0.0.1"},
		{"loopback v4 high", "127.255.255.254"},
		{"loopback v6", "::1"},
		{"link-local metadata", "169.254.169.254"},
		{"link-local v6", "fe80::1"},
		{"link-local multicast v4", "224.0.0.251"},
		{"link-local multicast v6", "ff02::1"},
		{"interface-local multicast v6", "ff01::1"},
		{"multicast v4", "239.1.2.3"},
		{"unique local v6", "fd00::1"},
		{"unique local v6 low", "fc00::1"},
		{"rfc1918 10", "10.0.0.5"},
		{"rfc1918 172 docker bridge", "172.17.0.1"},
		{"rfc1918 192.168", "192.168.1.10"},
		{"cgnat", "100.64.1.1"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v6", "::"},
		{"broadcast", "255.255.255.255"},
		{"reserved 240/4", "240.0.0.1"},
		{"this network", "0.1.2.3"},
		{"benchmarking", "198.18.0.1"},
		{"protocol assignments", "192.0.0.1"},
		{"nat64 to loopback", "64:ff9b::7f00:1"},
		{"ipv4-compatible loopback", "::127.0.0.1"},
		// IPv4-mapped IPv6 spellings of the same targets.
		{"mapped loopback", "::ffff:127.0.0.1"},
		{"mapped metadata", "::ffff:169.254.169.254"},
		{"mapped rfc1918", "::ffff:10.0.0.5"},
		{"mapped docker bridge", "::ffff:172.17.0.1"},
		{"mapped cgnat", "::ffff:100.64.1.1"},
	}

	for _, tc := range blocked {
		host := tc.addr
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}

		t.Run("literal "+tc.name, func(t *testing.T) {
			stubDNS(t, nil) // any lookup at all would be a bug here
			if _, err := ValidateSourceURL("http://" + host + "/latest/meta-data/"); err == nil {
				t.Fatalf("accepted literal %s", tc.addr)
			} else {
				wantField(t, err, "url")
			}
		})

		t.Run("resolved "+tc.name, func(t *testing.T) {
			stubDNS(t, map[string][]string{"evil.example": {tc.addr}})
			if _, err := ValidateSourceURL("http://evil.example/"); err == nil {
				t.Fatalf("accepted name resolving to %s", tc.addr)
			} else {
				wantField(t, err, "url")
			}
		})
	}
}

// A name answering with several records is only as safe as its worst one.
func TestValidateSourceURLRejectsAnyBadAddressInASet(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		ok    bool
	}{
		{"all public", []string{"93.184.216.34", "2606:4700::1"}, true},
		{"private last", []string{"93.184.216.34", "10.0.0.5"}, false},
		{"private first", []string{"127.0.0.1", "93.184.216.34"}, false},
		{"private in the middle", []string{"93.184.216.34", "169.254.169.254", "1.1.1.1"}, false},
		{"mapped loopback last", []string{"93.184.216.34", "::ffff:127.0.0.1"}, false},
		{"empty answer", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDNS(t, map[string][]string{"multi.example": tc.addrs})
			_, err := ValidateSourceURL("https://multi.example/v")
			if tc.ok && err != nil {
				t.Fatalf("rejected %v: %v", tc.addrs, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("accepted %v", tc.addrs)
			}
		})
	}
}

// Numeric host spellings that netip will not parse but an HTTP client will.
func TestValidateSourceURLRejectsLegacyNumericHosts(t *testing.T) {
	hosts := []string{
		"2130706433",   // 127.0.0.1 as a 32-bit integer
		"0177.0.0.1",   // octal
		"0x7f000001",   // hex
		"0x7f.0.0.1",   // mixed hex
		"127.1",        // short form
		"192.168.1",    // short form
		"169.254.169.", // trailing dot, still numeric
	}
	for _, h := range hosts {
		t.Run(h, func(t *testing.T) {
			stubDNS(t, nil)
			if _, err := ValidateSourceURL("http://" + h + "/"); err == nil {
				t.Fatalf("accepted numeric host %q", h)
			} else {
				wantField(t, err, "url")
			}
		})
	}
}

// Credentials in the URL must not disguise the host that is actually contacted.
func TestValidateSourceURLUsesTheRealHost(t *testing.T) {
	stubDNS(t, map[string][]string{"example.com": {"93.184.216.34"}})
	if _, err := ValidateSourceURL("http://example.com@169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("accepted a userinfo-disguised metadata URL")
	}
}

// The refusal must not tell the caller which address the name resolved to.
func TestValidateSourceURLErrorLeaksNoAddress(t *testing.T) {
	stubDNS(t, map[string][]string{"leaky.example": {"172.17.0.1"}})
	_, err := ValidateSourceURL("http://leaky.example/")
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if strings.Contains(err.Error(), "172.17.0.1") {
		t.Errorf("error leaks the resolved address: %v", err)
	}
}

// A caller's cancellation has to reach the resolver.
func TestValidateSourceURLContextHonoursCancellation(t *testing.T) {
	stubDNS(t, nil)
	lookupAddrs = func(ctx context.Context, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateSourceURLContext(ctx, "https://slow.example/x"); err == nil {
		t.Fatal("expected a rejection once the context was cancelled")
	}
}

// The preset tables feed argv directly; a value outside its codec's accepted
// range would be rejected if the same job were submitted as "custom".
func TestPresetTablesStayInsideCRFBounds(t *testing.T) {
	check := func(t *testing.T, label string, table map[string]map[string]int) {
		t.Helper()
		for name, byCodec := range table {
			for codec, crf := range byCodec {
				b, ok := CRFBounds[codec]
				if !ok {
					t.Errorf("%s %q references codec %q with no CRF bounds", label, name, codec)
					continue
				}
				if crf < b[0] || crf > b[1] {
					t.Errorf("%s %q sets crf %d for %s, outside %d..%d", label, name, crf, codec, b[0], b[1])
				}
			}
		}
	}
	check(t, "encode preset", EncodePresetCRF)
	check(t, "compress target", CompressCRF)

	for preset, speed := range EncodePresetSpeed {
		if err := oneOf("preset", preset, EncodePresets); err != nil {
			t.Errorf("preset %q is not offered to the UI", preset)
		}
		if err := oneOf("speed", speed, EncodeSpeeds); err != nil {
			t.Errorf("preset %q resolves to unknown speed %q", preset, speed)
		}
	}
}

// The UI renders its pickers straight from this payload.
func TestAllIsJSONSerialisableAndComplete(t *testing.T) {
	all := All()
	for _, key := range []string{
		"video_containers", "video_codecs", "encode_speeds", "resolutions",
		"frame_rates", "audio_codecs", "audio_bitrates", "encode_presets",
		"audio_formats", "image_formats", "compress_targets", "retention_choices",
		"crf_bounds", "target_size_bounds", "encode_preset_values",
		"compress_target_values", "image_compress_quality",
	} {
		if _, ok := all[key]; !ok {
			t.Errorf("All() is missing key %q", key)
		}
	}
	if _, err := json.Marshal(all); err != nil {
		t.Fatalf("All() does not marshal: %v", err)
	}
}

// The client writes these into its own selects, so each has to be on offer.
func TestCompressTargetTuneMatchesTheOptionLists(t *testing.T) {
	has := func(opts []Option, v string) bool {
		for _, o := range opts {
			if o.Value == v {
				return true
			}
		}
		return false
	}
	for _, target := range CompressTargets {
		tune, ok := CompressTargetTune[target.Value]
		if !ok {
			t.Errorf("no tuning for compress target %q", target.Value)
			continue
		}
		if !has(EncodeSpeeds, tune.Speed) {
			t.Errorf("%s: speed %q is not an encode speed", target.Value, tune.Speed)
		}
		if !has(Resolutions, tune.Resolution) {
			t.Errorf("%s: resolution %q is not offered", target.Value, tune.Resolution)
		}
		if !has(AudioBitrates, tune.AudioBitrate) {
			t.Errorf("%s: audio bitrate %q is not offered", target.Value, tune.AudioBitrate)
		}
	}
}
