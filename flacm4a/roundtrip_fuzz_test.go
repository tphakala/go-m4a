// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"testing"
)

// FuzzRoundTrip proves the one property FLAC guarantees: it is lossless, so for
// any interleaved PCM and any legal config, decode(encode(pcm)) is byte-identical
// to pcm. TestFLACRoundTrip pins that for the handful of sine clips someone wrote
// down; this pins it for arbitrary sample values, which is where a packing or
// stride bug in the 16- and 24-bit interleave paths would hide. 24-bit had no
// round-trip coverage in this package at all until #10 added one case, exactly
// the sort of hole a property fuzzer closes structurally rather than by memory.
//
// The config is derived from a selector byte rather than fuzzed field by field:
// only 1-2 channels, 16- or 24-bit, and compression 0-8 are legal, and a fuzzer
// left to invent a bit depth would spend its whole budget bouncing off the config
// guard instead of exercising the codec. The sample rate is fixed for the same
// reason. What is genuinely fuzzed is the sample bytes, which is the part with an
// invariant worth proving.
func FuzzRoundTrip(f *testing.F) {
	// Seed with valid 16-bit sine clips across both channel counts, plus a
	// selector that reaches the 24-bit and higher-compression paths.
	f.Add(genS16(256, 1), uint8(0))
	f.Add(genS16(256, 2), uint8(0x01))
	f.Add(genS16(300, 2), uint8(0x53)) // 24-bit, stereo, compression level 5

	f.Fuzz(func(t *testing.T, pcm []byte, sel uint8) {
		channels := int(sel&0x01) + 1 // 1 or 2
		bitDepth := 16
		if sel&0x02 != 0 {
			bitDepth = 24
		}
		compression := int(sel>>4) % 9 // 0..8

		// Trim to a whole number of interleaved samples; PCM that ends mid-sample
		// is a caller error the bridge rejects, not something round-trip applies to.
		stride := channels * (bitDepth / 8)
		pcm = pcm[:len(pcm)-len(pcm)%stride]
		if len(pcm) == 0 {
			t.Skip("no whole samples")
		}
		// Cap the clip so a single execution stays fast enough to fuzz densely; the
		// invariant does not depend on length beyond a frame or two.
		if len(pcm) > 1<<16 {
			t.Skip("clip too large for a fuzz execution")
		}

		cfg := Config{SampleRate: 48000, Channels: channels, BitDepth: bitDepth, CompressionLevel: compression}

		var buf memWS
		if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
			// A legal config over whole samples must always encode. A failure here
			// is a real bug, not an expected rejection.
			t.Fatalf("EncodeInterleaved(%d ch, %d-bit, %d bytes): %v", channels, bitDepth, len(pcm), err)
		}

		got, _, err := DecodeInterleaved(bytes.NewReader(buf.buf))
		if err != nil {
			t.Fatalf("DecodeInterleaved: %v", err)
		}
		if !bytes.Equal(got, pcm) {
			t.Fatalf("round-trip mismatch: got %d bytes, want %d (ch %d, %d-bit)", len(got), len(pcm), channels, bitDepth)
		}
	})
}
