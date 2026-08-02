package runner

import (
	"encoding/json"
	"testing"

	"aiofiles/internal/jobs"
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
