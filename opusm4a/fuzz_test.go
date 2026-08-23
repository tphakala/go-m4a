// SPDX-License-Identifier: MIT

package opusm4a

import (
	"math"
	"testing"

	"github.com/tphakala/go-m4a/internal/reservation"
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
		if got > reservation.MaxPCMReservation {
			t.Fatalf("reservation %d exceeds the %d ceiling", got, reservation.MaxPCMReservation)
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
