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

// encodeAllocCeiling bounds the encode path's allocations for the 15-second clip.
// Reslicing every frame out of one pre-sized arena, rather than a bytes.Clone per
// frame, holds this clip at ~104 allocations; the removed clones were ~175 of the
// old ~279 (one per frame, plus fixed pipeline cost from the encoder, the writer's
// sample tables, and StreamInfoBytes). The ceiling sits above the arena figure
// with headroom for minor pipeline drift, and well below the clone total, so a
// revert to per-frame cloning trips it.
const encodeAllocCeiling = 150

// TestEncodeInterleavedAllocations is the regression guard for that arena (#20:
// the repo keeps allocation-count tests so a growth-chain regression cannot slip
// back in invisibly). The sink is hoisted and reset the same way
// BenchmarkEncodeInterleaved does, so its own growth is not counted.
func TestEncodeInterleavedAllocations(t *testing.T) {
	for _, tc := range benchCases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := genS16(benchSamplesCh, tc.channels)
			cfg := Config{SampleRate: benchRate, Channels: tc.channels, BitDepth: 16, CompressionLevel: 5}
			w := &memWS{}
			if err := EncodeInterleaved(w, cfg, pcm); err != nil {
				t.Fatalf("warm-up encode: %v", err)
			}

			allocs := testing.AllocsPerRun(10, func() {
				w.pos = 0
				if err := EncodeInterleaved(w, cfg, pcm); err != nil {
					t.Fatalf("encode: %v", err)
				}
			})

			if allocs > encodeAllocCeiling {
				t.Errorf("encode allocates %.0f times, want <= %d (per-frame cloning may be back)", allocs, encodeAllocCeiling)
			}
		})
	}
}

// longFormSamplesCh is a clip whose decoded stereo PCM (~76 MiB) is past the
// reservation ceiling. The 15-second cases above live entirely inside the band
// where the reservation covers the whole output in one make; a real regression in
// the regrow path (the append chain that runs once the reservation is spent) was
// invisible to the whole suite because nothing decoded far enough to reach it
// (#20). This case reaches it.
const longFormSamplesCh = benchRate * 420 // 7 minutes

// BenchmarkDecodeInterleavedLongForm measures the decode path past the
// reservation ceiling, where the output grows by appending beyond the reserved
// buffer. It is stereo only, since that is what crosses the 64 MiB ceiling; the
// encode is hoisted, so only the regrow-heavy decode is timed.
func BenchmarkDecodeInterleavedLongForm(b *testing.B) {
	pcm := genS16(longFormSamplesCh, 2)
	cfg := Config{SampleRate: benchRate, Channels: 2, BitDepth: 16, CompressionLevel: 5}
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
}

// BenchmarkDecodeInterleavedUnknownLength measures the decode path when the
// stream declares an unknown length (TotalSamples=0), so the reservation never
// engages and the output is built entirely by appending. It is the case where any
// cost the reservation path adds is pure loss, and the shape a benchmark should
// watch so that cost stays visible (#20).
func BenchmarkDecodeInterleavedUnknownLength(b *testing.B) {
	pcm := genS16(benchSamplesCh, 2)
	cfg := Config{SampleRate: benchRate, Channels: 2, BitDepth: 16, CompressionLevel: 5}
	w := &memWS{}
	if err := EncodeInterleaved(w, cfg, pcm); err != nil {
		b.Fatalf("encode: %v", err)
	}
	enc := w.buf
	clearTotalSamples(b, enc, benchSamplesCh)

	b.SetBytes(int64(len(pcm)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := DecodeInterleaved(bytes.NewReader(enc)); err != nil {
			b.Fatalf("decode: %v", err)
		}
	}
}
