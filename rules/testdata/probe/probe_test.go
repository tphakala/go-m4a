package probe

// The matchers in rules/testing.go are gated on the _test.go file name, so
// their probes live here. This file is a test file only in name: `go test
// ./...` never reaches it (testdata), and rules_test.go only lints it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// rule: BenchmarkLoop
func BenchmarkLoop(b *testing.B) {
	for i := 0; i < b.N; i++ { // want "use for b.Loop() { ... } instead of for i := 0; i < b.N; i++"
		_ = i
	}
	for i := range b.N { // want "use for b.Loop() { ... } instead of for i := range b.N"
		_ = i
	}
	for range b.N { // want "use for b.Loop() { ... } instead of for range b.N"
		work()
	}
}

func work() {}

func take(context.Context, ...int) {}

// rule: TestingContext
func TestContext(t *testing.T) {
	ctx := context.Background() // want "t.Context() instead of context.Background() for automatic cancellation"
	ctx = context.TODO()        // want "t.Context() instead of context.TODO() for automatic cancellation"
	take(context.Background())  // want "t.Context() instead of context.Background() (Go 1.24+)"
	take(context.TODO(), 1)     // want "t.Context() instead of context.TODO() (Go 1.24+)"
	_ = ctx
}

// rule: TestingArtifactDir
func TestArtifactDir(t *testing.T) {
	_, _ = os.MkdirTemp("", "probe-*") // want "t.ArtifactDir()"
}

// rule: SynctestSleep
func TestSynctestSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Second) // want "synctest.Sleep(time.Second)"
		synctest.Wait()
	})
}

// rule: HttptestNewTestServer
func TestHttptestNewTestServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { // want "httptest.NewTestServer"
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
	})
}
