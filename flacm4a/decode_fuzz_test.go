// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// FuzzDecodeInterleaved hands attacker-controlled container bytes to the FLAC
// bridge, which feeds them to go-flac. It seeds from the interop FLAC-in-MP4
// fixtures so the fuzzer starts from bytes that actually reach the frame decoder
// rather than bouncing off the demuxer.
//
// The contract asserted is the bridge's own, not the codec's: over any input the
// decode never panics, and a decode that succeeds never returns more than
// m4a.DefaultMaxDecodedBytes of PCM. The size bound is the regression guard for
// the decode-amplification class (#18): a two-byte foreign packet can decode to
// far more than its size, and the ceiling is what stops that from becoming an
// unbounded allocation. A crash found here may be an upstream go-flac bug rather
// than a go-m4a one; the target still owns go-m4a's contract regardless of whose
// code the finding lands in.
func FuzzDecodeInterleaved(f *testing.F) {
	for _, name := range []string{"flac_mono44k.mp4", "flac_stereo48k.mp4"} {
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
			// A rejected file is a valid outcome. The frame decoder can only decode
			// what the demuxer accepted, so any error is fine as long as it did not
			// crash getting there.
			return
		}
		if len(pcm) > m4a.DefaultMaxDecodedBytes {
			t.Fatalf("decoded %d bytes, above the %d ceiling", len(pcm), m4a.DefaultMaxDecodedBytes)
		}
	})
}
