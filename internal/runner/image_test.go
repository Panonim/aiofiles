package runner

import (
	"testing"

	"aiofiles/internal/presets"
)

// docker/policy.xml carries the same limits, but it only exists inside the
// image: a local or dev run gets them from argv or not at all. Every one of
// these bounds a different way to blow the box up, so losing one silently is
// worth a test.
func TestMagickArgsCarriesResourceLimits(t *testing.T) {
	args := magickArgs("/tmp/in.png", "/tmp/out.webp", "webp",
		&presets.ImageParams{Format: "webp", Quality: 82, Width: 100}, false)

	for _, name := range []string{"memory", "map", "disk", "area", "width", "height", "list-length", "thread", "time"} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-limit" && args[i+1] == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv has no -limit %s: %q", name, args)
		}
	}
	// Limits are only honoured before the input is read.
	for i, a := range args {
		if a == "/tmp/in.png" {
			if i == 0 || args[i-1] != "60" {
				t.Errorf("limits do not all precede the input: %q", args)
			}
			break
		}
	}
}
