// SPDX-License-Identifier: MIT

package opusm4a

import (
	"math"
	"testing"
)

// FuzzPCMReservation asserts the invariants of the decode reservation over
// arbitrary integers rather than over the rows someone thought of. See
// flacm4a.FuzzPCMReservation for why the reservation is worth fuzzing at all;
// the arithmetic differs between the bridges but the contract does not.
func FuzzPCMReservation(f *testing.F) {
	f.Add(50, 2, 0)
	f.Add(0, 0, 0)
	f.Add(math.MaxInt, math.MaxInt, math.MaxInt)
	f.Add(math.MaxInt, 2, 1)
	f.Add(1, 1, -1)
	f.Add(7000, 2, 1<<20)

	f.Fuzz(func(t *testing.T, frameCount, channels, limit int) {
		got := pcmReservation(frameCount, channels, limit)

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

		// A limit is a ceiling, never a floor.
		unbounded := pcmReservation(frameCount, channels, 0)
		if limit > 0 && got > unbounded {
			t.Fatalf("limit %d raised the reservation from %d to %d", limit, unbounded, got)
		}
		if limit > 0 && limit >= unbounded && got != unbounded {
			t.Fatalf("limit %d is above the unbounded reservation %d but changed it to %d", limit, unbounded, got)
		}
	})
}

// FuzzShouldTrim mirrors flacm4a's: the trim decision must be total, and must
// never ask for a copy that reclaims less than the floor it is written against.
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
