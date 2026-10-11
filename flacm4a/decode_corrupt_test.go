// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"errors"
	"testing"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/testutil"
)

// TestDecodeCorruptFrameIsErrCorrupt is the deterministic guard for the decode
// error contract (#32): a container the demuxer accepts but whose FLAC payload the
// codec rejects must surface as a wrapped m4a.ErrCorrupt. The FuzzDecodeInterleaved
// sentinel assertion only bites under active fuzzing of that target, which no CI
// job runs, so without this test a revert of the ErrCorrupt wrapping would ship
// green. Here the first frame's sync bytes are zeroed while the sample table is
// left intact, so NewReader still parses the container and the error comes from
// the frame decoder, exactly the path the production wrap covers.
func TestDecodeCorruptFrameIsErrCorrupt(t *testing.T) {
	pcm := genS16(12000, 1)
	var buf testutil.WriteSeeker
	cfg := Config{SampleRate: 48000, Channels: 1, BitDepth: 16, CompressionLevel: 5}
	if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	data := buf.Bytes()
	start, end := testutil.MDATPayload(t, data)

	// Zero the first frame's leading bytes, destroying its sync code. The mdat
	// extent and the sample table are untouched, so the container still parses.
	for i := start; i < start+16 && i < end; i++ {
		data[i] = 0
	}

	// Pin the intent: the container must still parse, so the ErrCorrupt below comes
	// from the frame decoder rejecting the payload, not from a container-level
	// rejection. Without this a future stricter NewReader could pass the test for
	// the wrong reason.
	if _, err := m4a.NewReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("NewReader rejected the container, so the codec-decode path is not exercised: %v", err)
	}

	_, _, err := DecodeInterleaved(bytes.NewReader(data))
	if err == nil {
		t.Fatal("decode of a corrupt FLAC frame returned nil, want an error")
	}
	if !errors.Is(err, m4a.ErrCorrupt) {
		t.Errorf("decode error does not wrap ErrCorrupt: %v", err)
	}
}
