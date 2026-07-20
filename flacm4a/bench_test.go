// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"testing"
)

// benchClip is a 15-second 48 kHz clip, the shape BirdNET-Go exports.
const (
	benchRate      = 48000
	benchSeconds   = 15
	benchSamplesCh = benchRate * benchSeconds
)

// benchCases covers the two channel counts the bridge accepts. The allocation
// profile differs between them because the frame count depends only on the
// inter-channel sample count, while the PCM buffer scales with channel count.
var benchCases = []struct {
	name     string
	channels int
}{
	{"mono", 1},
	{"stereo", 2},
}

// Both benchmarks report MB/s over the PCM size rather than the file size, so
// the encode and decode figures are directly comparable. For decode that is
// bytes produced, not bytes read.

func BenchmarkEncodeInterleaved(b *testing.B) {
	for _, tc := range benchCases {
		b.Run(tc.name, func(b *testing.B) {
			pcm := genS16(benchSamplesCh, tc.channels)
			cfg := Config{SampleRate: benchRate, Channels: tc.channels, BitDepth: 16, CompressionLevel: 5}

			// Hoist the sink and warm it up to the final file size, the same shape
			// aacm4a's benchmarks use. memWS.Write reallocates on every extending
			// write, so a fresh sink per iteration puts its own O(N^2) growth
			// inside the measured region, where it accounted for over 90% of the
			// reported B/op and buried the bridge's own allocations entirely.
			w := &memWS{}
			if err := EncodeInterleaved(w, cfg, pcm); err != nil {
				b.Fatalf("warm-up encode: %v", err)
			}

			b.SetBytes(int64(len(pcm)))
			b.ReportAllocs()
			for b.Loop() {
				w.pos = 0
				if err := EncodeInterleaved(w, cfg, pcm); err != nil {
					b.Fatalf("encode: %v", err)
				}
			}
		})
	}
}

func BenchmarkDecodeInterleaved(b *testing.B) {
	for _, tc := range benchCases {
		b.Run(tc.name, func(b *testing.B) {
			pcm := genS16(benchSamplesCh, tc.channels)
			cfg := Config{SampleRate: benchRate, Channels: tc.channels, BitDepth: 16, CompressionLevel: 5}
			w := &memWS{}
			if err := EncodeInterleaved(w, cfg, pcm); err != nil {
				b.Fatalf("encode: %v", err)
			}
			enc := w.buf

			b.SetBytes(int64(len(pcm)))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := DecodeInterleaved(bytes.NewReader(enc)); err != nil {
					b.Fatalf("decode: %v", err)
				}
			}
		})
	}
}
