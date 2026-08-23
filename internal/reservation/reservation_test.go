// SPDX-License-Identifier: MIT

package reservation

import (
	"math"
	"testing"
)

// TestShouldTrim pins the trim policy directly, which is the only affordable way
// to cover it: the cases that matter are streams of tens of megabytes, and the
// regression it guards against was found by measurement rather than by a test.
// The slack figures below are the ones measured on real decodes. This test lives
// here rather than in each bridge because both bridges share this one policy.
func TestShouldTrim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		length   int
		slack    int
		wantTrim bool
	}{
		// The case the trim exists for: a file declared far more audio than it
		// carried, so the reservation dwarfs the result.
		{"over-declared length", 2000, 129070, true},
		{"over-declared at the ceiling", 2000, MaxPCMReservation - 2000, true},
		// Pins the firing side. Without a row here the policy could be loosened
		// several times over, retaining multiples of the audio, with a green
		// suite: the rows above clear any plausible threshold by so much that
		// they cannot distinguish one from another.
		{"slack over half the audio", 1000000, 600000, true},
		// Honest streams. A buffer that grew by append carries up to about a
		// quarter of its length as headroom; none of that is worth a copy, and
		// charging for it measurably cost more than pre-sizing ever saved. The
		// slack figures are Go's real growth for these lengths from a zero
		// reservation, and the 30s row is the binding one at ratio 0.2501, which
		// is what rules out a quarter as the divisor.
		{"exact reservation", 2880000, 0, false},
		{"unknown length, 5s", 960000, 236032, false},
		{"unknown length, 30s", 5760000, 1440768, false},
		// Past the ceiling the buffer starts at MaxPCMReservation and grows from
		// there, so its residual slack is a much smaller fraction than an
		// unknown-length stream's: this is a real 9-minute clip at ratio 0.0115.
		{"declared length past the ceiling, 9min", 103680000, 1191936, false},
		// Just under the proportional boundary, which is where a moderately
		// over-declared file (a truncated recording is the realistic one) lands.
		// Its slack is retained by design; recovering it would cost copying the
		// whole buffer to reclaim a third of it.
		{"moderate over-declaration is retained", 38400000, 15360000, false},
		// Small absolute slack is never worth a copy whatever the proportion.
		{"tiny buffer, proportionally huge slack", 8, 1024, false},
		{"empty buffer", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ShouldTrim(tc.length, tc.length+tc.slack); got != tc.wantTrim {
				t.Errorf("ShouldTrim(len=%d, cap=%d) = %v, want %v (slack %d)",
					tc.length, tc.length+tc.slack, got, tc.wantTrim, tc.slack)
			}
		})
	}
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
		if got := ShouldTrim(length, capacity); got && capacity-length <= MaxRetainedSlack {
			t.Fatalf("ShouldTrim(%d, %d) asked for a copy to reclaim %d bytes, under the %d floor",
				length, capacity, capacity-length, MaxRetainedSlack)
		}
	})
}
