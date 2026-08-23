// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"testing"
)

// benchClip is a 15-second 48 kHz clip, the shape BirdNET-Go exports. opusm4a had
// no benchmarks at all until #20, despite being the bridge with the worst
// allocation profile of the three; these give its encode and decode paths the
// same coverage flacm4a and aacm4a already have.
const (
	benchRate      = 48000
	benchSeconds   = 15
	benchSamplesCh = benchRate * benchSeconds
)

var benchCases = []struct {
	name     string
	channels int
}{
	{"mono", 1},
	{"stereo", 2},
}

// Both benchmarks report MB/s over the PCM size rather than the file size, so the
// encode and decode figures are directly comparable. For decode that is bytes
// produced, not bytes read.

func BenchmarkEncodeInterleaved(b *testing.B) {
	for _, tc := range benchCases {
		b.Run(tc.name, func(b *testing.B) {
			pcm := genSine(benchSamplesCh, tc.channels)
			cfg := Config{SampleRate: benchRate, Channels: tc.channels, Bitrate: 96000}

			// Hoist the sink and warm it up to the final file size, the same shape the
			// flacm4a and aacm4a benchmarks use, so memWS's own extending-write growth
			// stays out of the measured region.
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
			pcm := genSine(benchSamplesCh, tc.channels)
			cfg := Config{SampleRate: benchRate, Channels: tc.channels, Bitrate: 96000}
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
