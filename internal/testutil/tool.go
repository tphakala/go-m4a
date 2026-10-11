// SPDX-License-Identifier: MIT

package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// RequireFFmpegEnv names the variable that, set to any non-empty value, turns a
// missing ffmpeg or ffprobe from a skip into a failure in every interop test
// that finds its tool with LookTool. CI sets it for the whole job in the step
// that installs ffmpeg on Linux, so the cross-checks cannot pass there by never
// running. Jobs without ffmpeg (macOS, Windows, 386) leave it unset and skip.
const RequireFFmpegEnv = "M4A_REQUIRE_FFMPEG"

// LookTool returns the path of the named tool on PATH. It is meant for ffmpeg
// and ffprobe, the tools RequireFFmpegEnv covers. When the tool is missing,
// LookTool skips the test, or fails it if RequireFFmpegEnv is set.
// New interop tests must use it rather than exec.LookPath followed by t.Skip,
// which would pass silently on CI when the tool is missing.
func LookTool(tb testing.TB, name string) string {
	tb.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv(RequireFFmpegEnv) != "" {
			tb.Fatalf("%s not on PATH but %s is set: %v", name, RequireFFmpegEnv, err)
		} else {
			tb.Skipf("%s not on PATH", name)
		}
	}
	return path
}
