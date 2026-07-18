// SPDX-License-Identifier: MIT

package box

import (
	"encoding/binary"
	"testing"
)

// benchTableEntries is the per-sample/per-chunk table length the box benchmarks
// marshal and parse. It matches the frame count of a realistic short clip, where
// the stsz carries one entry per access unit.
const benchTableEntries = 5000

// benchSizes builds a realistic per-sample size table (100..800 bytes each).
func benchSizes(n int) []uint32 {
	sizes := make([]uint32, n)
	for i := range sizes {
		sizes[i] = uint32(100 + (i*97)%701)
	}
	return sizes
}

// BenchmarkAppendStsz marshals a large stsz onto a nil destination, the way
// buildMoov effectively does when it appends the sample table onto the partial
// stbl slice. It captures the growslice cost of an un-presized destination.
func BenchmarkAppendStsz(b *testing.B) {
	sizes := benchSizes(benchTableEntries)
	b.ReportAllocs()
	for b.Loop() {
		_ = AppendStsz(nil, sizes)
	}
}

// BenchmarkAppendStszPregrown marshals the same table into a destination that is
// pre-sized to the exact box length and reused across iterations. It is the
// mechanical-fix baseline: with a known-size destination the marshal is
// allocation-free.
func BenchmarkAppendStszPregrown(b *testing.B) {
	sizes := benchSizes(benchTableEntries)
	dst := make([]byte, 0, 12+4+4+4*len(sizes))
	b.ReportAllocs()
	for b.Loop() {
		_ = AppendStsz(dst[:0], sizes)
	}
}

// buildStszBody returns an stsz FullBox body (the bytes ParseStsz consumes) for
// a per-sample table of n entries.
func buildStszBody(n int) []byte {
	sizes := benchSizes(n)
	return AppendStsz(nil, sizes)[8:] // strip the 8-byte box header
}

// buildStcoBody returns an stco FullBox body of n chunk offsets.
func buildStcoBody(n int) []byte {
	offsets := make([]uint32, n)
	for i := range offsets {
		offsets[i] = uint32(1000 + i*800)
	}
	return AppendStco(nil, offsets)[8:]
}

// buildStscBody returns a synthetic stsc FullBox body of n strictly-increasing
// entries (one sample per chunk), exercising the ParseStsc per-entry loop.
func buildStscBody(n int) []byte {
	body := make([]byte, 8+12*n)
	binary.BigEndian.PutUint32(body[4:], uint32(n)) // entry_count
	for i := 0; i < n; i++ {
		o := 8 + 12*i
		binary.BigEndian.PutUint32(body[o:], uint32(i+1)) // first_chunk, increasing
		binary.BigEndian.PutUint32(body[o+4:], 1)         // samples_per_chunk
		binary.BigEndian.PutUint32(body[o+8:], 1)         // sample_description_index
	}
	return body
}

// BenchmarkParseSampleTables parses the three per-open size/chunk tables. Each
// runs once per NewReader over the whole table, so these measure open-time cost,
// not per-frame streaming cost.
func BenchmarkParseSampleTables(b *testing.B) {
	stsz := buildStszBody(benchTableEntries)
	stco := buildStcoBody(benchTableEntries)
	stsc := buildStscBody(benchTableEntries)

	b.Run("Stsz", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _, _, err := ParseStsz(stsz)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Stco", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := ParseStco(stco); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Stsc", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := ParseStsc(stsc); err != nil {
				b.Fatal(err)
			}
		}
	})
}
