// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"math"
	"testing"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"
)

// TestEstimateADTSSizeIsClose checks the reservation against what the encoder
// actually produces for clips of at least a second, where the contract is
// zero regrows: the estimate must never be below the real stream, and must not
// exceed it by more than the per-case bound. Mainstream bitrates hold 1.15;
// very low bitrates get 1.35, because there the per-frame floor term dominates
// and the absolute waste is a few hundred bytes. Durations deliberately include
// fractional seconds: an earlier version rounded the duration up to whole
// seconds, and a test suite of whole-second clips certified it while it
// over-reserved fractional clips by up to 2.9x.
func TestEstimateADTSSizeIsClose(t *testing.T) {
	tests := []struct {
		name     string
		cfg      aacpcm.Config
		samples  int
		maxRatio float64
	}{
		{"mono 48k 96kbps 3s", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}, 144000, 1.15},
		{"stereo 48k 128kbps 2s", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2, Bitrate: 128000}, 96000, 1.15},
		{"stereo 44.1k default bitrate 2s", aacpcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2}, 88200, 1.15},
		{"mono 44.1k 64kbps 1s", aacpcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 1, Bitrate: 64000}, 44100, 1.15},
		{"mono 48k 96kbps 1.1s", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}, 52800, 1.15},
		{"stereo 44.1k 128kbps 1.5s", aacpcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2, Bitrate: 128000}, 66150, 1.15},
		{"stereo 48k default bitrate 2.5s", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}, 120000, 1.15},
		{"stereo 44.1k 32kbps 1s", aacpcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2, Bitrate: 32000}, 44100, 1.35},
		{"mono 48k 16kbps 1.5s", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 16000}, 72000, 1.35},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pcm := chirpS16(tc.samples, tc.cfg.Channels, tc.cfg.SampleRate)

			var adts bytes.Buffer
			if err := aacpcm.EncodeInterleaved(&adts, tc.cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}
			actual := adts.Len()
			estimate := estimateADTSSize(tc.cfg, tc.samples)

			ratio := float64(estimate) / float64(actual)
			t.Logf("estimate %d, actual %d, ratio %.4f", estimate, actual, ratio)
			// The whole point of the reservation is that the buffer never grows for
			// ordinary clips, so an estimate below the real size defeats it even
			// though it "looks close".
			if estimate < actual {
				t.Errorf("estimate %d is below the actual %d bytes, so the buffer still reallocates",
					estimate, actual)
			}
			if ratio > tc.maxRatio {
				t.Errorf("estimate %d for an actual %d bytes (ratio %.4f) is above %.2f",
					estimate, actual, ratio, tc.maxRatio)
			}
		})
	}
}

// TestEstimateADTSSizeShortClips pins the degraded-mode contract for clips too
// short for the estimate to cover exactly: the reservation may undershoot, but
// never below half the real stream, so bytes.Buffer's doubling growth fixes it
// in a single regrow. Short clips and extreme bitrates are exactly the inputs
// the estimate is allowed to get wrong cheaply, and exactly the inputs earlier
// test suites never exercised.
func TestEstimateADTSSizeShortClips(t *testing.T) {
	configs := []aacpcm.Config{
		{SampleRate: 44100, BitDepth: 16, Channels: 2, Bitrate: 16000},
		{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 24000},
		{SampleRate: 44100, BitDepth: 16, Channels: 1, Bitrate: 320000},
		{SampleRate: 48000, BitDepth: 16, Channels: 2, Bitrate: 320000},
		{SampleRate: 48000, BitDepth: 16, Channels: 2},
	}
	for _, cfg := range configs {
		for _, ms := range []int{20, 50, 100, 300, 500} {
			samples := cfg.SampleRate * ms / 1000
			pcm := chirpS16(samples, cfg.Channels, cfg.SampleRate)

			var adts bytes.Buffer
			if err := aacpcm.EncodeInterleaved(&adts, cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved (%+v, %dms): %v", cfg, ms, err)
			}
			actual := adts.Len()
			estimate := estimateADTSSize(cfg, samples)
			if estimate <= 0 {
				t.Errorf("%dkbps ch%d %dms: estimate %d, want positive", cfg.Bitrate/1000, cfg.Channels, ms, estimate)
			}
			if 2*estimate < actual {
				t.Errorf("%dkbps ch%d %dms: estimate %d is under half the actual %d bytes, costing more than one regrow",
					cfg.Bitrate/1000, cfg.Channels, ms, estimate, actual)
			}
		}
	}
}

// TestEstimateADTSSizeEdgeCases covers the inputs that must not overflow, panic,
// or produce a nonsensical reservation.
func TestEstimateADTSSizeEdgeCases(t *testing.T) {
	base := aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}

	if got := estimateADTSSize(base, 0); got != 0 {
		t.Errorf("estimate for an empty clip = %d, want 0", got)
	}
	if got := estimateADTSSize(base, -1); got != 0 {
		t.Errorf("estimate for a negative sample count = %d, want 0", got)
	}
	if got := estimateADTSSize(base, 1); got <= 0 {
		t.Errorf("estimate for a single sample = %d, want a positive reservation", got)
	}

	// Every field the arithmetic divides or multiplies by must be refused rather
	// than trusted, because this runs before go-aac validates the config. A
	// negative result would panic bytes.Buffer.Grow.
	for _, tc := range []struct {
		name string
		cfg  aacpcm.Config
	}{
		{"zero sample rate", aacpcm.Config{SampleRate: 0, BitDepth: 16, Channels: 1}},
		{"negative sample rate", aacpcm.Config{SampleRate: -48000, BitDepth: 16, Channels: 1}},
		{"zero channels", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 0}},
		{"negative channels", aacpcm.Config{SampleRate: 48000, BitDepth: -8, Channels: -1}},
		{"more channels than supported", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 64}},
	} {
		if got := estimateADTSSize(tc.cfg, 48000); got != 0 {
			t.Errorf("%s: estimate = %d, want 0", tc.name, got)
		}
	}

	// A bitrate far above the AAC bit reservoir must clamp to the ceiling rather
	// than reserve gigabytes. go-aac clamps the encode itself, so the estimate
	// must not take the caller's number at face value.
	absurd := base
	absurd.Bitrate = math.MaxInt32
	const samples = 48000
	// The frame count includes the aac.EncoderDelay priming samples, matching
	// what the encoder actually emits.
	frames := int64((samples + aac.EncoderDelay + aac.FrameSize - 1) / aac.FrameSize)
	ceiling := int(frames * (maxFrameBytesPerChannel*int64(absurd.Channels) + adtsHeaderLen))
	if got := estimateADTSSize(absurd, samples); got != ceiling {
		t.Errorf("estimate for an absurd bitrate = %d, want the ceiling %d", got, ceiling)
	}

	// An absurd bitrate must not overflow the intermediate product. This needs a
	// clip of several seconds: with a one-second clip the seconds factor is 1 and
	// the multiply cannot overflow whatever the bitrate, which is exactly how an
	// earlier version of this test passed while the code still panicked.
	const longClipSeconds = 30
	longClip := 48000 * longClipSeconds
	longFrameCount := int64((longClip + aac.EncoderDelay + aac.FrameSize - 1) / aac.FrameSize)
	longClipCeiling := int(longFrameCount * (maxFrameBytesPerChannel + adtsHeaderLen))
	// math.MaxInt keeps these in range on a 32-bit build too.
	for _, bitrate := range []int{math.MaxInt / 4, math.MaxInt / 2, math.MaxInt} {
		got := estimateADTSSize(aacpcm.Config{
			SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: bitrate,
		}, longClip)
		if got < 0 {
			t.Fatalf("estimate for bitrate %d = %d; a negative panics bytes.Buffer.Grow", bitrate, got)
		}
		if got != longClipCeiling {
			t.Errorf("estimate for bitrate %d = %d, want the ceiling %d", bitrate, got, longClipCeiling)
		}
	}

	// Zero Bitrate means go-aac's own default, not a zero-size reservation.
	zeroRate := base
	zeroRate.Bitrate = 0
	withDefault := estimateADTSSize(zeroRate, samples)
	if withDefault <= 0 {
		t.Fatalf("estimate with the default bitrate = %d, want positive", withDefault)
	}
	if want := defaultBitrate / 8 * samples / zeroRate.SampleRate; withDefault < want {
		t.Errorf("estimate %d is below the %d bytes the default bitrate implies", withDefault, want)
	}

	// A long clip must not overflow into a negative or wrapped value. Asserting
	// bounds rather than an exact figure keeps this from re-implementing the
	// formula it is testing: the properties that matter are that the result stays
	// positive, never exceeds the bit-reservoir ceiling for that many frames, and
	// fits the int a caller will pass to Grow.
	const longSamples = math.MaxInt32
	longFrames := int64((longSamples + aac.EncoderDelay + aac.FrameSize - 1) / aac.FrameSize)
	longCeiling := longFrames * (maxFrameBytesPerChannel*int64(base.Channels) + adtsHeaderLen)
	got := estimateADTSSize(base, longSamples)
	switch {
	case got <= 0:
		t.Errorf("estimate for a very long clip = %d, want positive", got)
	case int64(got) > longCeiling:
		t.Errorf("estimate for a very long clip = %d, above the ceiling %d", got, longCeiling)
	case got > math.MaxInt32:
		t.Errorf("estimate for a very long clip = %d, above MaxInt32", got)
	}

	// Saturation of the sample-count input: only reachable where int is 64-bit.
	// Build the counts at runtime, because a constant expression this large does
	// not compile on a 32-bit build even inside a branch that never runs there.
	// A clip longer than the input pin must reserve the same as one exactly at
	// it, because the pin happens before any arithmetic sees the value; the
	// MaxInt case used to wrap the +1023 frame round-up instead and drive the
	// whole estimate negative.
	if math.MaxInt > math.MaxInt32 {
		atPin := estimateADTSSize(base, math.MaxInt32)
		if atPin <= 0 {
			t.Fatalf("estimate at the input pin = %d, want positive", atPin)
		}
		huge := math.MaxInt32
		huge = huge / 2 * 9 // a few billion samples, well past the input pin
		if got := estimateADTSSize(base, huge); got != atPin {
			t.Errorf("estimate for a %d sample clip = %d, want the saturated %d", huge, got, atPin)
		}
		if got := estimateADTSSize(base, math.MaxInt); got != atPin {
			t.Errorf("estimate for a MaxInt sample clip = %d, want the saturated %d", got, atPin)
		}
	}

	// A sample rate near MaxInt used to wrap the duration arithmetic: the
	// samples+rate-1 round-up numerator overflowed int64, the duration came out
	// negative, and the negative estimate panicked bytes.Buffer.Grow on a config
	// go-aac rejects with a clean error. The window is narrow (samples plus rate
	// has to land within a couple past MaxInt64), so probe several small counts
	// against several extreme rates rather than one pair.
	for _, rate := range []int{math.MaxInt, math.MaxInt - 1, math.MaxInt - 2} {
		for samples := 1; samples <= 5; samples++ {
			cfg := aacpcm.Config{SampleRate: rate, BitDepth: 16, Channels: 1}
			if got := estimateADTSSize(cfg, samples); got < 0 {
				t.Errorf("estimate for rate %d with %d samples = %d; a negative panics bytes.Buffer.Grow",
					rate, samples, got)
			}
		}
	}
}

// TestEstimateADTSSizeNeverNegative sweeps the cross product of hostile values
// for every input the arithmetic touches. The one property that keeps callers
// safe is that the result is always in 0..MaxInt32: bytes.Buffer.Grow panics on
// a negative, and anything larger is not an int on a 32-bit build. Sweeping the
// full cross product, rather than hand-picking pairs, is the point: every one
// of the three historical breaks of this function came from an input
// combination the author had argued could not matter.
func TestEstimateADTSSizeNeverNegative(t *testing.T) {
	extremes := []int{
		math.MinInt, math.MinInt + 1, math.MinInt / 2, -48001, -1, 0, 1, 2, 3, 7, 8,
		1024, 44100, 48000, math.MaxInt32 - 1, math.MaxInt32, math.MaxInt / 2, math.MaxInt - 1, math.MaxInt,
	}
	channelValues := []int{math.MinInt, -1, 0, 1, 2, 3, 64, math.MaxInt}
	for _, sampleRate := range extremes {
		for _, channels := range channelValues {
			for _, bitrate := range extremes {
				for _, samples := range extremes {
					cfg := aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Bitrate: bitrate}
					got := estimateADTSSize(cfg, samples)
					if got < 0 || int64(got) > math.MaxInt32 {
						t.Fatalf("estimateADTSSize(%+v, %d) = %d, outside 0..MaxInt32", cfg, samples, got)
					}
				}
			}
		}
	}
}

// TestReservationDoesNotChangeEncodedBytes pins that the reservation is a pure
// performance change. The reference is an encode into a buffer that was never
// grown, so the two differ in exactly one thing: whether the capacity was
// reserved up front.
func TestReservationDoesNotChangeEncodedBytes(t *testing.T) {
	cfg := aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	const samplesPerChannel = 48000
	pcm := chirpS16(samplesPerChannel, cfg.Channels, cfg.SampleRate)

	var unreserved bytes.Buffer
	if err := aacpcm.EncodeInterleaved(&unreserved, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved (unreserved): %v", err)
	}
	var reserved bytes.Buffer
	reserved.Grow(estimateADTSSize(cfg, samplesPerChannel))
	if err := aacpcm.EncodeInterleaved(&reserved, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved (reserved): %v", err)
	}

	if unreserved.Len() == 0 {
		t.Fatal("encode produced nothing")
	}
	if !bytes.Equal(unreserved.Bytes(), reserved.Bytes()) {
		t.Fatalf("reserving %d bytes changed the encoded stream (%d vs %d bytes)",
			estimateADTSSize(cfg, samplesPerChannel), unreserved.Len(), reserved.Len())
	}
}

// TestEncodeInterleavedSurvivesHostileConfig is the regression test for the crash
// the reservation introduced: estimateADTSSize ran on an unvalidated config and
// could return a negative count, which panics bytes.Buffer.Grow. Every config here
// must come back as an ordinary error (or succeed), never as a panic, exactly as
// it did before the reservation existed.
func TestEncodeInterleavedSurvivesHostileConfig(t *testing.T) {
	tests := []struct {
		name   string
		cfg    aacpcm.Config
		pcmLen int // 0 means the ten-second default below
	}{
		// Two negatives cancel in the stride, so this slips past the caller's
		// stride guard and used to drive the ceiling negative.
		{"negative channels and bit depth", aacpcm.Config{SampleRate: 48000, BitDepth: -8, Channels: -1}, 0},
		{"negative channels", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: -1}, 0},
		{"zero channels", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 0}, 0},
		{"zero sample rate", aacpcm.Config{SampleRate: 0, BitDepth: 16, Channels: 1}, 0},
		{"negative sample rate", aacpcm.Config{SampleRate: -48000, BitDepth: 16, Channels: 1}, 0},
		{"negative bitrate", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: -1}, 0},
		// go-aac clamps an over-ceiling bitrate rather than rejecting it, so this
		// config encodes successfully. It used to overflow the estimate to a
		// negative and crash a call that had always worked.
		{"bitrate that overflows the estimate", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: math.MaxInt / 2}, 0},
		{"maximum bitrate", aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: math.MaxInt}, 0},
		// A maximal sample rate needs a TINY clip to bite, not a long one: the
		// duration round-up adds the rate to the sample count, and that sum only
		// wraps int64 when the count lands within a couple of the gap to MaxInt64.
		// Two 16-bit samples put it there exactly. go-aac rejects the rate with a
		// clean error, so the only wrong outcome is a panic before it gets to.
		{"maximum sample rate", aacpcm.Config{SampleRate: math.MaxInt, BitDepth: 16, Channels: 1}, 4},
		{"near-maximum sample rate", aacpcm.Config{SampleRate: math.MaxInt - 1, BitDepth: 16, Channels: 1}, 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EncodeInterleaved panicked on %s: %v", tc.name, r)
				}
			}()
			var ws memWriteSeeker
			// Ten seconds of mono s16 by default. The length matters: with a
			// sub-second clip the seconds factor in the estimate is 0 or 1 and the
			// multiply that overflowed cannot be exercised at all, which is how the
			// first version of this test passed against code that still panicked.
			pcmLen := tc.pcmLen
			if pcmLen == 0 {
				pcmLen = 48000 * 10 * 2
			}
			pcm := make([]byte, pcmLen)
			// The error (or its absence) is the encoder's business; this test only
			// asserts the call returns rather than crashing the process.
			_ = EncodeInterleaved(&ws, tc.cfg, pcm)
		})
	}
}
