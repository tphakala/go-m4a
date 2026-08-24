// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"testing"
)

// segmentTotalBytes sums the payload of a frame set, the byte figure a caller
// would pass to Grow.
func segmentTotalBytes(frames [][]byte) int {
	total := 0
	for _, au := range frames {
		total += len(au)
	}
	return total
}

// TestGrowPreventsFirstSegmentRegrow is the point of Grow: a writer sized up
// front for the first segment must buffer that whole segment without reallocating
// any of its three arenas. Capacities are captured after Grow and must be
// unchanged once all frames are buffered.
func TestGrowPreventsFirstSegmentRegrow(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	frames := synthFrames(94) // about two seconds of AAC-LC at 48 kHz
	f.Grow(len(frames), segmentTotalBytes(frames))

	samplesCap := cap(f.samples)
	sizesCap := cap(f.sizes)
	durationsCap := cap(f.durations)

	for _, au := range frames {
		if err := f.WriteFrameDuration(au, 1024); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}

	if cap(f.samples) != samplesCap {
		t.Errorf("sample arena regrew after Grow: %d -> %d", samplesCap, cap(f.samples))
	}
	if cap(f.sizes) != sizesCap {
		t.Errorf("sizes table regrew after Grow: %d -> %d", sizesCap, cap(f.sizes))
	}
	if cap(f.durations) != durationsCap {
		t.Errorf("durations table regrew after Grow: %d -> %d", durationsCap, cap(f.durations))
	}
}

// TestFirstSegmentRegrowsWithoutGrow is the counterpart: without Grow, the first
// segment does pay the growth chain. It keeps TestGrowPreventsFirstSegmentRegrow
// honest by proving the arenas start empty and genuinely grow.
func TestFirstSegmentRegrowsWithoutGrow(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	if cap(f.samples) != 0 {
		t.Fatalf("fresh writer sample arena has capacity %d, want 0", cap(f.samples))
	}
	for _, au := range synthFrames(94) {
		if err := f.WriteFrameDuration(au, 1024); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	if cap(f.samples) == 0 {
		t.Error("sample arena never grew while buffering a segment")
	}
}

// TestGrowReachesRequestedCapacity covers the pooled path a fresh-writer test
// misses: when the writer already carries capacity (a prior stream's, retained
// across Reset), a second Grow to a larger segment must still reach the requested
// capacity rather than stopping short. slices.Grow subtracts unused capacity
// itself, so the shortfall handed to it must be measured against len, not against
// unused capacity.
func TestGrowReachesRequestedCapacity(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	// Seed a modest retained capacity the way a prior segment would leave behind.
	f.Grow(64, 4096)
	if cap(f.sizes) == 0 || cap(f.samples) == 0 {
		t.Fatalf("seed Grow reserved nothing: sizes %d, samples %d", cap(f.sizes), cap(f.samples))
	}

	// Now ask for a larger segment. The result must cover the request fully.
	const wantSamples, wantBytes = 512, 128 * 1024
	f.Grow(wantSamples, wantBytes)
	if cap(f.sizes) < wantSamples {
		t.Errorf("sizes cap %d < requested %d (unused capacity double-counted?)", cap(f.sizes), wantSamples)
	}
	if cap(f.durations) < wantSamples {
		t.Errorf("durations cap %d < requested %d", cap(f.durations), wantSamples)
	}
	if cap(f.samples) < wantBytes {
		t.Errorf("samples cap %d < requested %d (unused capacity double-counted?)", cap(f.samples), wantBytes)
	}
}

// TestGrowIsPureCapacityHint pins that Grow changes only capacity, never output:
// two writers fed identical frames, one grown and one not, must emit byte-identical
// segments. This is the invariant that lets Grow be documented as a hint that
// cannot affect correctness.
func TestGrowIsPureCapacityHint(t *testing.T) {
	for _, n := range []int{1, 5, 94, 200} {
		frames := synthFrames(n)

		plain, err := NewFragmentWriter(aacFragmentConfig())
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}
		grown, err := NewFragmentWriter(aacFragmentConfig())
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}
		grown.Grow(len(frames), segmentTotalBytes(frames))

		got := segmentFrames(t, plain, nil, frames)
		want := segmentFrames(t, grown, nil, frames)
		if !bytes.Equal(got, want) {
			t.Errorf("n=%d: grown writer produced a different segment than the plain one", n)
		}
	}
}

// TestGrowNegativePanics documents the one caller error Grow rejects, matching
// slices.Grow and bytes.Buffer.Grow.
func TestGrowNegativePanics(t *testing.T) {
	tests := []struct {
		name             string
		samples, byteLen int
	}{
		{"negative samples", -1, 0},
		{"negative bytes", 0, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewFragmentWriter(aacFragmentConfig())
			if err != nil {
				t.Fatalf("NewFragmentWriter: %v", err)
			}
			defer func() {
				if recover() == nil {
					t.Errorf("Grow(%d, %d) did not panic", tc.samples, tc.byteLen)
				}
			}()
			f.Grow(tc.samples, tc.byteLen)
		})
	}
}

// TestGrowClampedToSegmentCaps proves a wild hint cannot drive an OOM-scale
// speculative reservation: Grow clamps both arguments to the per-segment caps.
func TestGrowClampedToSegmentCaps(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	f.Grow(1<<30, 1<<30)
	if cap(f.samples) > maxSegmentBytes {
		t.Errorf("sample arena cap %d exceeds maxSegmentBytes %d", cap(f.samples), maxSegmentBytes)
	}
	if cap(f.sizes) > maxSamplesPerSegment {
		t.Errorf("sizes cap %d exceeds maxSamplesPerSegment %d", cap(f.sizes), maxSamplesPerSegment)
	}
	if cap(f.durations) > maxSamplesPerSegment {
		t.Errorf("durations cap %d exceeds maxSamplesPerSegment %d", cap(f.durations), maxSamplesPerSegment)
	}
}

// TestResetReleasesOversizedArena covers the retention bound: an arena grown past
// resetRetainBytes by a pathological segment is released on Reset instead of
// pinning that peak for the life of a pool.
func TestResetReleasesOversizedArena(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	f.Grow(0, resetRetainBytes*2)
	if cap(f.samples) <= resetRetainBytes {
		t.Fatalf("Grow did not produce an oversized arena: cap %d", cap(f.samples))
	}
	if err := f.Reset(aacFragmentConfig()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if cap(f.samples) != 0 {
		t.Errorf("Reset kept an oversized arena: cap %d, want 0", cap(f.samples))
	}

	// The same for the parallel sample tables.
	f.Grow(resetRetainSamples*2, 0)
	if cap(f.sizes) <= resetRetainSamples {
		t.Fatalf("Grow did not produce oversized tables: cap %d", cap(f.sizes))
	}
	if err := f.Reset(aacFragmentConfig()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if cap(f.sizes) != 0 || cap(f.durations) != 0 {
		t.Errorf("Reset kept oversized tables: sizes %d, durations %d, want 0", cap(f.sizes), cap(f.durations))
	}
}

// TestResetRetainsNormalArena is the other half of the bound: a normal segment
// stays well under the retention limit, so a pooled writer keeps its capacity and
// still stops allocating after the first stream. A follow-up segment must allocate
// nothing.
func TestResetRetainsNormalArena(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	frames := synthFrames(94)
	buf := segmentFrames(t, f, nil, frames)

	samplesCap := cap(f.samples)
	if samplesCap == 0 || samplesCap > resetRetainBytes {
		t.Fatalf("normal arena cap %d is not within the retained band (0, %d]", samplesCap, resetRetainBytes)
	}
	if err := f.Reset(aacFragmentConfig()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if cap(f.samples) != samplesCap {
		t.Errorf("Reset dropped a normal arena: %d -> %d", samplesCap, cap(f.samples))
	}

	allocs := testing.AllocsPerRun(20, func() {
		for _, au := range frames {
			if err := f.WriteFrame(au); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
		}
		var err error
		if buf, err = f.AppendSegment(buf[:0]); err != nil {
			t.Fatalf("AppendSegment: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("segment after Reset allocates %.0f times, want 0", allocs)
	}
}
