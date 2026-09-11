package runner

import (
	"encoding/json"
	"testing"

	"aiofiles/internal/jobs"
	"aiofiles/internal/presets"
)

// A compress job takes no audio codec from the client, so the container has to
// supply one ffmpeg can actually mux.
func TestCompressSpecPicksAudioCodecFromContainer(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   []string
	}{
		{
			"webm gets opus",
			`{"container":"webm","video_codec":"vp9"}`,
			[]string{
				"-hide_banner", "-nostdin", "-y",
				"-i", "/tmp/in.mkv",
				"-progress", "pipe:1", "-nostats",
				"-c:v", "libvpx-vp9",
				"-cpu-used", "2", "-row-mt", "1",
				"-crf", "33", "-b:v", "0",
				"-c:a", "libopus", "-b:a", "128k",
				"/tmp/out.webm",
			},
		},
		{
			"mp4 keeps aac",
			`{"container":"mp4","video_codec":"h264"}`,
			[]string{
				"-hide_banner", "-nostdin", "-y",
				"-i", "/tmp/in.mkv",
				"-progress", "pipe:1", "-nostats",
				"-c:v", "libx264",
				"-preset", "medium",
				"-crf", "24",
				"-c:a", "aac", "-b:a", "128k",
				"-movflags", "+faststart",
				"/tmp/out.webm",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &FFmpeg{}
			spec, err := r.specFor(&jobs.Job{Type: jobs.TypeCompress, Params: json.RawMessage(tc.params)})
			if err != nil {
				t.Fatalf("specFor(%s) = %v", tc.params, err)
			}
			got := ffmpegArgs("/tmp/in.mkv", "/tmp/out.webm", spec)
			if !equalStrings(got, tc.want) {
				t.Errorf("argv =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestEditArgs(t *testing.T) {
	tests := []struct {
		name   string
		params string
		input  string
		want   []string
	}{
		{
			"a video trim re-encodes so the cut is exact",
			`{"start":3.5,"end":10}`,
			"/tmp/in.mkv",
			[]string{
				"-hide_banner", "-nostdin", "-y",
				"-ss", "3.500",
				"-i", "/tmp/in.mkv",
				"-progress", "pipe:1", "-nostats",
				"-t", "6.500",
				"-c:v", "libx264", "-preset", "veryfast", "-crf", "18",
				"-pix_fmt", "yuv420p", "-c:a", "copy",
				"/tmp/out.mkv",
			},
		},
		{
			"an audio file has nothing to re-encode",
			`{"start":1,"end":2}`,
			"/tmp/in.mp3",
			[]string{
				"-hide_banner", "-nostdin", "-y",
				"-ss", "1.000",
				"-i", "/tmp/in.mp3",
				"-progress", "pipe:1", "-nostats",
				"-t", "1.000",
				"-c", "copy", "-avoid_negative_ts", "make_zero",
				"/tmp/out.mp3",
			},
		},
		{
			"crop re-encodes the video only",
			`{"crop_x":10,"crop_y":20,"crop_w":640,"crop_h":480}`,
			"/tmp/in.mp4",
			[]string{
				"-hide_banner", "-nostdin", "-y",
				"-i", "/tmp/in.mp4",
				"-progress", "pipe:1", "-nostats",
				"-vf", "crop=640:480:10:20",
				"-c:v", "libx264", "-preset", "veryfast", "-crf", "18",
				"-pix_fmt", "yuv420p", "-c:a", "copy",
				"-movflags", "+faststart",
				"/tmp/out.mp4",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := presets.ParseEdit(json.RawMessage(tc.params), 7)
			if err != nil {
				t.Fatalf("ParseEdit(%s) = %v", tc.params, err)
			}
			container, copyOnly := editPlan(tc.input)
			got := editArgs(tc.input, "/tmp/out."+container, p, copyOnly)
			if !equalStrings(got, tc.want) {
				t.Errorf("argv =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// An edit cannot stay in a container H.264 does not mux, so it lands in MKV.
func TestEditPlan(t *testing.T) {
	tests := []struct {
		input     string
		container string
		copyOnly  bool
	}{
		{"/tmp/in.webm", "mkv", false},
		{"/tmp/in.mov", "mp4", false},
		{"/tmp/in.flac", "flac", true},
		{"/tmp/in", "mp4", false},
	}
	for _, tc := range tests {
		container, copyOnly := editPlan(tc.input)
		if container != tc.container || copyOnly != tc.copyOnly {
			t.Errorf("editPlan(%q) = %q, %v; want %q, %v",
				tc.input, container, copyOnly, tc.container, tc.copyOnly)
		}
	}
}
