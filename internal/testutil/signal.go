// SPDX-License-Identifier: MIT

package testutil

import (
	"encoding/binary"
	"math"
)

// ChannelS16 returns channel ch of little-endian 16-bit interleaved PCM as
// float64 samples. A trailing partial frame is ignored.
func ChannelS16(pcm []byte, channels, ch int) []float64 {
	stride := channels * 2
	out := make([]float64, 0, len(pcm)/stride)
	for off := ch * 2; off+2 <= len(pcm); off += stride {
		out = append(out, float64(int16(binary.LittleEndian.Uint16(pcm[off:]))))
	}
	return out
}

// NormXCorr is the normalized cross-correlation of a against b at the given
// lag, aligning a[n] with b[n+lag]. The result is in [-1, 1] and is invariant
// to a constant amplitude scale, which is what makes it a clean alignment probe
// for a lossy codec that need not preserve absolute level.
func NormXCorr(a, b []float64, lag int) float64 {
	var dot, na, nb float64
	for n := range a {
		m := n + lag
		if m < 0 || m >= len(b) {
			continue
		}
		dot += a[n] * b[m]
		na += a[n] * a[n]
		nb += b[m] * b[m]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
