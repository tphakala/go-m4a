// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/reservation"
)

// encodeFile encodes PCM to a FLAC .mp4 and returns the file bytes.
func encodeFile(t *testing.T, pcm []byte, channels int) []byte {
	t.Helper()
	var buf memWS
	cfg := Config{SampleRate: 48000, Channels: channels, BitDepth: 16, CompressionLevel: 5}
	if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	return buf.buf
}

// encodeSilence returns a FLAC .mp4 holding samplesPerCh samples of digital
// silence per channel, together with the byte length that decodes back to.
// Silence is the ordinary input that compresses hardest, which is what makes the
// decode limit worth having: five seconds of 48 kHz stereo is a couple of
// kilobytes in the file and 960,000 bytes decoded.
func encodeSilence(t *testing.T, samplesPerCh, channels int) (file []byte, decoded int) {
	t.Helper()
	pcm := make([]byte, samplesPerCh*channels*2)
	return encodeFile(t, pcm, channels), len(pcm)
}

// TestDecodeInterleavedLimitRejectsOversizedDecode is the regression guard for
// the decompression-bomb shape in #18. Silence encodes to FLAC constant
// subframes, so the file is a small fraction of what it decodes to. The ratio is
// asserted as well, so that an encoder change which stopped producing an
// amplifying fixture surfaces here instead of leaving the test asserting nothing.
func TestDecodeInterleavedLimitRejectsOversizedDecode(t *testing.T) {
	file, decoded := encodeSilence(t, 48000*5, 2)
	ratio := decoded / max(len(file), 1)
	if ratio < 100 {
		t.Fatalf("fixture is not amplifying enough to test the bound: %d bytes decode to %d (%dx)", len(file), decoded, ratio)
	}
	t.Logf("fixture: %d bytes decode to %d bytes (%dx amplification)", len(file), decoded, ratio)

	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(file), decoded/4)
	if !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit", err)
	}
	if pcm != nil {
		t.Errorf("PCM = %d bytes, want nil when the limit stops the decode", len(pcm))
	}
}

// TestDecodeInterleavedLimitAcceptsExactFit pins the boundary: a limit equal to
// the decoded size is a fit, not an excess.
func TestDecodeInterleavedLimitAcceptsExactFit(t *testing.T) {
	file, decoded := encodeSilence(t, 4096*3, 1)

	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(file), decoded)
	if err != nil {
		t.Fatalf("DecodeInterleavedLimit(exact): %v", err)
	}
	if len(pcm) != decoded {
		t.Errorf("PCM = %d bytes, want %d", len(pcm), decoded)
	}
}

// TestDecodeInterleavedLimitOneByteShort is the other half of the boundary: one
// byte under the decoded size must fail.
func TestDecodeInterleavedLimitOneByteShort(t *testing.T) {
	file, decoded := encodeSilence(t, 4096*3, 1)

	if _, _, err := DecodeInterleavedLimit(bytes.NewReader(file), decoded-1); !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit", err)
	}
}

func TestDecodeInterleavedLimitNonPositiveMeansUnlimited(t *testing.T) {
	pcm := genS16(12000, 2)
	file := encodeFile(t, pcm, 2)

	for _, limit := range []int{0, -1} {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(file), limit)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit(%d): %v", limit, err)
		}
		if !bytes.Equal(got, pcm) {
			t.Errorf("limit %d: round-trip PCM mismatch: got %d bytes, want %d bytes", limit, len(got), len(pcm))
		}
	}
}

// TestPCMReservationRespectsLimit covers the other half of the bound: a limit
// must also cap what the decoder reserves up front. Without it a hostile
// self-description would still drive a large speculative allocation before the
// first frame decoded, which is most of what the limit exists to prevent. The
// reservation is not observable through the exported API, so this asserts on the
// function directly.
func TestPCMReservationRespectsLimit(t *testing.T) {
	t.Parallel()
	// A stream declaring far more audio than the bounded cases allow, so the limit
	// rather than the declaration decides each one.
	const (
		totalSamples = 48000 * 600 // 10 minutes
		frameCount   = 7000
		channels     = 2
		bitDepth     = 16
	)
	// Ten minutes of 48 kHz stereo 16-bit is 115.2 MB, so with no limit this
	// declaration is what reservation.MaxPCMReservation is there to clamp. Asserting the exact
	// value rather than "positive" keeps the limit cases below meaningful: if the
	// clamp broke, an unasserted unbounded value would still be positive and every
	// bounded case would still pass.
	unbounded := pcmReservation(totalSamples, frameCount, channels, channels, bitDepth, 0)
	if unbounded != reservation.MaxPCMReservation {
		t.Fatalf("unbounded reservation = %d, want the %d ceiling for a declaration this large", unbounded, reservation.MaxPCMReservation)
	}

	for _, limit := range []int{1, 4096, 1 << 20} {
		got := pcmReservation(totalSamples, frameCount, channels, channels, bitDepth, limit)
		if got > limit {
			t.Errorf("pcmReservation(limit=%d) = %d, want no more than the limit", limit, got)
		}
		if got < 0 {
			t.Errorf("pcmReservation(limit=%d) = %d, want a non-negative capacity", limit, got)
		}
	}

	// A limit above what the stream declares must not inflate the reservation: the
	// limit is a ceiling, never a floor.
	if got := pcmReservation(totalSamples, frameCount, channels, channels, bitDepth, math.MaxInt); got != unbounded {
		t.Errorf("pcmReservation with a limit above the declaration = %d, want the unbounded value %d", got, unbounded)
	}
}

func TestDecodeStreamDeliversEveryFrame(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			pcm := genS16(12000, channels)
			file := encodeFile(t, pcm, channels)

			var got []byte
			frames := 0
			info, err := DecodeStream(bytes.NewReader(file), func(frame []byte) error {
				frames++
				got = append(got, frame...)
				return nil
			})
			if err != nil {
				t.Fatalf("DecodeStream: %v", err)
			}
			if info.Codec != m4a.CodecFLAC {
				t.Errorf("Codec = %v, want FLAC", info.Codec)
			}
			// Guards the assertion below from going vacuous if the fixture ever
			// shrinks to a single frame.
			if info.FrameCount < 2 {
				t.Fatalf("fixture has %d frames, want a multi-frame stream", info.FrameCount)
			}
			if frames != info.FrameCount {
				t.Errorf("callback ran %d times, want %d (one per access unit)", frames, info.FrameCount)
			}
			// FLAC is lossless, so the concatenated frames must be the input exactly.
			if !bytes.Equal(got, pcm) {
				t.Errorf("streamed PCM mismatch: got %d bytes, want %d bytes", len(got), len(pcm))
			}
		})
	}
}

func TestDecodeStreamPropagatesCallbackError(t *testing.T) {
	// 20000 samples at the encoder's 4096-sample blocks is five frames, so a
	// stopAt below the frame count really does stop the decode early: at exactly
	// three frames the stopAt = 3 row would also be what running to EOF produces.
	file := encodeFile(t, genS16(20000, 1), 1)
	stop := errors.New("caller stopped the decode")

	// Stopping partway matters as much as stopping at once: an implementation that
	// only checked the first callback's error would pass a first-frame-only test.
	for _, stopAt := range []int{1, 3} {
		calls := 0
		_, err := DecodeStream(bytes.NewReader(file), func([]byte) error {
			calls++
			if calls == stopAt {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) {
			t.Fatalf("stopAt %d: err = %v, want the callback's error", stopAt, err)
		}
		if calls != stopAt {
			t.Errorf("callback ran %d times, want %d: the decode must stop at the first error", calls, stopAt)
		}
	}
}

func TestDecodeStreamRejectsNilCallback(t *testing.T) {
	file, _ := encodeSilence(t, 4096*3, 1)

	if _, err := DecodeStream(bytes.NewReader(file), nil); err == nil {
		t.Error("DecodeStream(nil callback) returned nil error, want a rejection rather than a panic")
	}
}

// TestDecodeInterleavedAppliesTheDefaultLimit pins the wiring that is the whole
// point of the change: the convenience wrapper must delegate with the package
// default rather than with no limit. Asserting it against the real ceiling would
// cost a gigabyte-scale decode, so the ceiling is lowered for the duration of the
// test instead. Without this, a wrapper delegating with 0 passes every other test
// in the file.
func TestDecodeInterleavedAppliesTheDefaultLimit(t *testing.T) {
	if got := defaultMaxDecodedBytes.Load(); got != m4a.DefaultMaxDecodedBytes {
		t.Fatalf("defaultMaxDecodedBytes = %d, want the package constant %d", got, m4a.DefaultMaxDecodedBytes)
	}
	file, decoded := encodeSilence(t, 48000*5, 2)

	restore := defaultMaxDecodedBytes.Load()
	t.Cleanup(func() { defaultMaxDecodedBytes.Store(restore) })

	defaultMaxDecodedBytes.Store(int64(decoded / 4))
	if _, _, err := DecodeInterleaved(bytes.NewReader(file)); !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit: DecodeInterleaved must delegate with the package default", err)
	}

	// And the same file decodes once the ceiling is above it, so the failure above
	// is the limit rather than anything else about the fixture.
	defaultMaxDecodedBytes.Store(int64(decoded))
	pcm, _, err := DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved under a sufficient default: %v", err)
	}
	if len(pcm) != decoded {
		t.Errorf("PCM = %d bytes, want %d", len(pcm), decoded)
	}
}
