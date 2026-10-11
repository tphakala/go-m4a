// SPDX-License-Identifier: MIT

package testutil

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
)

func TestChannelS16(t *testing.T) {
	put := func(vals ...int16) []byte {
		b := make([]byte, 2*len(vals))
		for i, v := range vals {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
		}
		return b
	}
	// Three stereo frames plus a trailing partial frame holding only a left
	// sample, which channel 0 keeps and channel 1 lacks.
	stereo := put(1, -2, 3, -4, 5, -6, 7)
	if got, want := ChannelS16(stereo, 2, 0), []float64{1, 3, 5, 7}; !slices.Equal(got, want) {
		t.Errorf("ch0 = %v, want %v", got, want)
	}
	if got, want := ChannelS16(stereo, 2, 1), []float64{-2, -4, -6}; !slices.Equal(got, want) {
		t.Errorf("ch1 = %v, want %v", got, want)
	}
	if got, want := ChannelS16(put(9, -8, 7), 1, 0), []float64{9, -8, 7}; !slices.Equal(got, want) {
		t.Errorf("mono = %v, want %v", got, want)
	}
}

func TestNormXCorr(t *testing.T) {
	sig := make([]float64, 64)
	for i := range sig {
		sig[i] = math.Sin(float64(i)*0.7) + 0.3*math.Sin(float64(i)*2.9)
	}
	neg := make([]float64, len(sig))
	scaled := make([]float64, len(sig))
	for i, v := range sig {
		neg[i] = -v
		scaled[i] = 3 * v
	}
	const eps = 1e-9
	if got := NormXCorr(sig, sig, 0); math.Abs(got-1) > eps {
		t.Errorf("self at lag 0 = %v, want 1", got)
	}
	if got := NormXCorr(sig, neg, 0); math.Abs(got+1) > eps {
		t.Errorf("negation at lag 0 = %v, want -1", got)
	}
	if got := NormXCorr(sig, scaled, 0); math.Abs(got-1) > eps {
		t.Errorf("scaled copy at lag 0 = %v, want 1 (gain invariance)", got)
	}
	if got := NormXCorr(make([]float64, 8), sig, 0); got != 0 {
		t.Errorf("zero vector = %v, want 0", got)
	}

	// b is a delayed by k samples, so b[n+k] == a[n]: the peak is at lag k.
	const k = 3
	delayed := make([]float64, len(sig))
	copy(delayed[k:], sig)
	best, bestLag := math.Inf(-1), 0
	for lag := -4; lag <= 4; lag++ {
		if c := NormXCorr(sig, delayed, lag); c > best {
			best, bestLag = c, lag
		}
	}
	if bestLag != k {
		t.Errorf("peak lag = %d, want %d", bestLag, k)
	}
}
