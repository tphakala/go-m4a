// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// FuzzDecodeInterleaved hands attacker-controlled container bytes to the Opus
// bridge, which feeds them to go-opus. It seeds from the interop Opus-in-MP4
// fixtures so the fuzzer starts from bytes that actually reach the decoder rather
// than bouncing off the demuxer.
//
// The contract asserted is the bridge's own, not the codec's: over any input the
// decode never panics, and a decode that succeeds never returns more than
// m4a.DefaultMaxDecodedBytes of PCM. That ceiling is load-bearing for Opus in
// particular: a two-byte packet of zero-length DTX frames decodes to the 120 ms
// maximum, so a foreign file's per-packet amplification is exactly what the limit
// exists to bound. A crash found here may be an upstream go-opus bug rather than a
// go-m4a one; the target still owns go-m4a's contract regardless.
func FuzzDecodeInterleaved(f *testing.F) {
	for _, name := range []string{"opus_mono48k.mp4", "opus_stereo48k.mp4"} {
		path := filepath.Join("..", "testdata", "interop", name)
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // fixed test fixture path
			f.Add(b)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic on %d-byte input: %v", len(data), p)
			}
		}()

		pcm, _, err := DecodeInterleaved(bytes.NewReader(data))
		if err != nil {
			return
		}
		if len(pcm) > m4a.DefaultMaxDecodedBytes {
			t.Fatalf("decoded %d bytes, above the %d ceiling", len(pcm), m4a.DefaultMaxDecodedBytes)
		}
	})
}
