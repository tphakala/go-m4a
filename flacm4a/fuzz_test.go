// SPDX-License-Identifier: MIT

package flacm4a

import (
	"math"
	"testing"
)

// FuzzPCMReservation asserts the invariants of the decode reservation over
// arbitrary integers rather than over the rows someone thought of.
//
// The function is a pure function of untrusted numbers: every argument except
// the caller's limit comes from a file's own self-description or from a sample
// table an attacker controls the shape of. Its result goes straight into a make,
// so a negative or absurd value is a panic or an allocation the file asked for,
// and the table test next door can only ever cover the combinations it lists.
// The invariants below are what the callers actually depend on:
//
//   - it never panics, whatever the arithmetic is handed;
//   - the result is a legal capacity, so 0 <= n and make can take it;
//   - the result never exceeds maxPCMReservation, which is what stops a
//     self-description from driving the allocation;
//   - the result never exceeds a positive caller limit, which is what stops it
//     from exceeding what the caller allowed;
//   - a limit is a ceiling and never a floor, so raising one cannot make the
//     reservation grow past what the file justified.
func FuzzPCMReservation(f *testing.F) {
	// Seeds: the shapes the table test pins, plus the edges that the table
	// cannot express as literals on both 32- and 64-bit builds.
	f.Add(uint64(48000*600), 7000, 2, 2, 16, 0)
	f.Add(uint64(0), 0, 0, 0, 0, 0)
	f.Add(uint64(math.MaxUint64), math.MaxInt, math.MaxInt, math.MaxInt, math.MaxInt, math.MaxInt)
	f.Add(uint64(math.MaxUint64), math.MaxInt, 8, 8, 32, 1)
	f.Add(uint64(1), 1, 1, 1, 8, -1)
	f.Add(uint64(4096), 1, 2, 0, 24, 1<<20)

	f.Fuzz(func(t *testing.T, totalSamples uint64, frameCount, siChannels, seChannels, bitDepth, limit int) {
		got := pcmReservation(totalSamples, frameCount, siChannels, seChannels, bitDepth, limit)

		if got < 0 {
			t.Fatalf("reservation %d is negative, which make would reject", got)
		}
		if got > maxPCMReservation {
			t.Fatalf("reservation %d exceeds the %d ceiling", got, maxPCMReservation)
		}
		if limit > 0 && got > limit {
			t.Fatalf("reservation %d exceeds the caller's limit %d", got, limit)
		}
		// The result is used as a capacity, so prove it is one.
		_ = make([]byte, 0, got)

		// A limit is a ceiling, never a floor: removing it, or raising it, can only
		// leave the reservation where the file's own description put it.
		unbounded := pcmReservation(totalSamples, frameCount, siChannels, seChannels, bitDepth, 0)
		if limit > 0 && got > unbounded {
			t.Fatalf("limit %d raised the reservation from %d to %d", limit, unbounded, got)
		}
		if limit > 0 && limit >= unbounded && got != unbounded {
			t.Fatalf("limit %d is above the unbounded reservation %d but changed it to %d", limit, unbounded, got)
		}
	})
}

// FuzzFrameReservation asserts that the encoder's frame-count estimate is a
// legal slice capacity for any input, including the values whose usual
// round-up form would overflow.
func FuzzFrameReservation(f *testing.F) {
	f.Add(0)
	f.Add(1)
	f.Add(4096)
	f.Add(4097)
	f.Add(math.MaxInt)
	f.Add(-1)

	f.Fuzz(func(t *testing.T, samplesPerChannel int) {
		got := frameReservation(samplesPerChannel)

		if got < 0 {
			t.Fatalf("frame reservation %d is negative, which make would reject", got)
		}
		// Every sample has to land in some frame, so the count can never be short
		// of the exact division; and it is a count of blocks, so it can never
		// exceed the sample count itself.
		if samplesPerChannel > 0 {
			if want := samplesPerChannel / encoderBlockSize; got < want {
				t.Fatalf("frame reservation %d is short of the %d whole blocks in %d samples", got, want, samplesPerChannel)
			}
			if got > samplesPerChannel {
				t.Fatalf("frame reservation %d exceeds the sample count %d", got, samplesPerChannel)
			}
		}
	})
}

// FuzzShouldTrim asserts that the trim decision is total and self-consistent:
// it must not panic on any length and capacity a decode can produce, and it
// must never ask for a copy of a buffer that has no slack to reclaim.
func FuzzShouldTrim(f *testing.F) {
	f.Add(0, 0)
	f.Add(1000000, 1600000)
	f.Add(math.MaxInt, math.MaxInt)
	f.Add(0, math.MaxInt)

	f.Fuzz(func(t *testing.T, length, capacity int) {
		if length < 0 || capacity < length {
			t.Skip("not a slice: a capacity is never below its length")
		}
		if got := shouldTrim(length, capacity); got && capacity-length <= maxRetainedSlack {
			t.Fatalf("shouldTrim(%d, %d) asked for a copy to reclaim %d bytes, under the %d floor",
				length, capacity, capacity-length, maxRetainedSlack)
		}
	})
}
