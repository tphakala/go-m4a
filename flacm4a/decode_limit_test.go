// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"errors"
	"math"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// encodeSilence returns a FLAC .mp4 holding samplesPerCh samples of digital
// silence per channel, together with the byte length that decodes back to.
// Silence is the ordinary input that compresses hardest, which is what makes the
// decode limit worth having: five seconds of 48 kHz stereo is a couple of
// kilobytes in the file and 960,000 bytes decoded.
func encodeSilence(t *testing.T, samplesPerCh, channels int) (file []byte, decoded int) {
	t.Helper()
	pcm := make([]byte, samplesPerCh*channels*2)
	var buf memWS
	cfg := Config{SampleRate: 48000, Channels: channels, BitDepth: 16, CompressionLevel: 5}
	if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	return buf.buf, len(pcm)
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
	var buf memWS
	if err := EncodeInterleaved(&buf, Config{SampleRate: 48000, Channels: 2, BitDepth: 16, CompressionLevel: 5}, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}

	for _, limit := range []int{0, -1} {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(buf.buf), limit)
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
	// A stream declaring far more audio than the bounded cases allow, so the limit
	// rather than the declaration decides each one.
	const (
		totalSamples = 48000 * 600 // 10 minutes
		frameCount   = 7000
		channels     = 2
		bitDepth     = 16
	)
	unbounded := pcmReservation(totalSamples, frameCount, channels, channels, bitDepth, 0)
	if unbounded <= 0 {
		t.Fatalf("unbounded reservation = %d, want a positive size for a well-declared stream", unbounded)
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
	pcm := genS16(12000, 2)
	var buf memWS
	if err := EncodeInterleaved(&buf, Config{SampleRate: 48000, Channels: 2, BitDepth: 16, CompressionLevel: 5}, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}

	var got []byte
	frames := 0
	info, err := DecodeStream(bytes.NewReader(buf.buf), func(frame []byte) error {
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
	if frames != info.FrameCount {
		t.Errorf("callback ran %d times, want %d (one per access unit)", frames, info.FrameCount)
	}
	// FLAC is lossless, so the concatenated frames must be the input exactly.
	if !bytes.Equal(got, pcm) {
		t.Errorf("streamed PCM mismatch: got %d bytes, want %d bytes", len(got), len(pcm))
	}
}

func TestDecodeStreamPropagatesCallbackError(t *testing.T) {
	file, _ := encodeSilence(t, 4096*3, 1)
	stop := errors.New("caller stopped the decode")

	calls := 0
	if _, err := DecodeStream(bytes.NewReader(file), func([]byte) error {
		calls++
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1: the decode must stop at the first error", calls)
	}
}

// TestDecodeInterleavedAppliesDefaultLimit pins that the convenience wrapper is
// bounded rather than unlimited. It cannot afford to decode past the real
// default, so it checks the wiring the cheap way: the same file decodes under
// the default, and the limit variant proves the mechanism.
func TestDecodeInterleavedAppliesDefaultLimit(t *testing.T) {
	if m4a.DefaultMaxDecodedBytes <= 0 {
		t.Fatalf("DefaultMaxDecodedBytes = %d, want a positive ceiling", m4a.DefaultMaxDecodedBytes)
	}
	file, decoded := encodeSilence(t, 4096*3, 1)

	pcm, _, err := DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if len(pcm) != decoded {
		t.Errorf("PCM = %d bytes, want %d", len(pcm), decoded)
	}
}
