// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"errors"
	"testing"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/testutil"
)

// TestDecodeCorruptPacketIsErrCorrupt is the deterministic guard for the decode
// error contract (#32): a container the demuxer accepts but whose Opus payload the
// codec rejects must surface as a wrapped m4a.ErrCorrupt. The FuzzDecodeInterleaved
// sentinel assertion only bites under active fuzzing of that target, which no CI
// job runs, so without this test a revert of the ErrCorrupt wrapping would ship
// green. Opus tolerates most byte flips (they decode to garbage without error), so
// the first packet's TOC is overwritten with a code-3 header declaring zero frames,
// which libopus rejects as an invalid packet. The sample table is left intact, so
// NewReader still parses the container and the error comes from the packet decoder,
// exactly the path the production wrap covers.
func TestDecodeCorruptPacketIsErrCorrupt(t *testing.T) {
	pcm := genSine(48000, 1) // one second, mono
	var buf testutil.WriteSeeker
	cfg := Config{SampleRate: 48000, Channels: 1, Bitrate: 96000}
	if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	data := buf.Bytes()
	start, end := testutil.MDATPayload(t, data)
	if start+2 > end {
		t.Fatalf("mdat payload too short to corrupt: [%d,%d)", start, end)
	}

	// TOC byte 0x83 is config 16, code 3 (an arbitrary frame count follows); a
	// following frame-count byte of 0x00 declares zero frames, which opus_packet_parse
	// rejects. This overwrites in place, so the packet size and sample table are
	// unchanged and the container still parses.
	data[start] = 0x83
	data[start+1] = 0x00

	// Pin the intent: the container must still parse, so the ErrCorrupt below comes
	// from the packet decoder rejecting the payload, not from a container-level
	// rejection. Without this a future stricter NewReader could pass the test for
	// the wrong reason.
	if _, err := m4a.NewReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("NewReader rejected the container, so the codec-decode path is not exercised: %v", err)
	}

	_, _, err := DecodeInterleaved(bytes.NewReader(data))
	if err == nil {
		t.Fatal("decode of a corrupt Opus packet returned nil, want an error")
	}
	if !errors.Is(err, m4a.ErrCorrupt) {
		t.Errorf("decode error does not wrap ErrCorrupt: %v", err)
	}
}
