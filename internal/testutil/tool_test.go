// SPDX-License-Identifier: MIT

package testutil

import (
	"fmt"
	"runtime"
	"testing"
)

// recordTB stands in for the test LookTool is given. Fatalf and Skipf record the
// outcome and end the goroutine with runtime.Goexit, as the real ones do, so the
// code after them in LookTool never runs. Every other method panics through the
// nil embedded TB, which would flag LookTool calling something unexpected.
type recordTB struct {
	testing.TB

	fatal, skipped string
}

func (*recordTB) Helper() {}

func (r *recordTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recordTB) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func runLookTool(name string) *recordTB {
	rec := &recordTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		LookTool(rec, name)
	}()
	<-done
	return rec
}

const missingTool = "go-m4a-no-such-tool"

func TestLookToolMissingSkips(t *testing.T) {
	t.Setenv(RequireFFmpegEnv, "")
	rec := runLookTool(missingTool)
	if rec.skipped == "" || rec.fatal != "" {
		t.Fatalf("missing tool without %s: skipped %q, fatal %q; want a skip", RequireFFmpegEnv, rec.skipped, rec.fatal)
	}
}

func TestLookToolMissingFailsWhenRequired(t *testing.T) {
	t.Setenv(RequireFFmpegEnv, "1")
	rec := runLookTool(missingTool)
	if rec.fatal == "" || rec.skipped != "" {
		t.Fatalf("missing tool with %s set: skipped %q, fatal %q; want a failure", RequireFFmpegEnv, rec.skipped, rec.fatal)
	}
}
