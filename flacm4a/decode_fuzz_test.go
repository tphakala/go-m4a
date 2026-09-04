// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"errors"
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
// decode never panics, a decode that succeeds never returns more than
// m4a.DefaultMaxDecodedBytes of PCM, and a decode that fails returns one of the
// bridge's typed sentinels rather than a bare codec error. The size bound is the
// regression guard for the decode-amplification class (#18): a two-byte foreign
// packet can decode to far more than its size, and the ceiling is what stops that
// from becoming an unbounded allocation. The sentinel assertion is the guard for
// #32: a container the demuxer accepted but whose FLAC payload the codec rejects
// must surface as ErrCorrupt, matching the demuxer's own rejections, so a caller
// can branch on the error type without matching bridge-internal strings. A crash
// found here may be an upstream go-flac bug rather than a go-m4a one; the target
// still owns go-m4a's contract regardless of whose code the finding lands in.
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
			// A rejected file is a valid outcome, but it must be typed: every decode
			// error, whether from the demuxer or from go-flac rejecting the payload,
			// wraps one of these sentinels.
			if !errors.Is(err, m4a.ErrCorrupt) && !errors.Is(err, m4a.ErrUnsupported) && !errors.Is(err, m4a.ErrDecodeLimit) && !errors.Is(err, m4a.ErrBoxTooLarge) {
				t.Fatalf("decode error on %d-byte input is not a typed sentinel: %v", len(data), err)
			}
			return
		}
		if len(pcm) > m4a.DefaultMaxDecodedBytes {
			t.Fatalf("decoded %d bytes, above the %d ceiling", len(pcm), m4a.DefaultMaxDecodedBytes)
		}
	})
}
