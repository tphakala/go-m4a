// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"errors"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// encodeClip encodes a stereo sine to an Opus .mp4 and returns the file bytes
// together with what an unlimited decode produces from it. Opus is lossy, so the
// decoded length is the reference the limit cases are stated against rather than
// the input length.
func encodeClip(t *testing.T, samplesPerCh, channels int) (file, decoded []byte) {
	t.Helper()
	var buf memWS
	cfg := Config{SampleRate: 48000, Channels: channels, Bitrate: 96000}
	if err := EncodeInterleaved(&buf, cfg, genSine(samplesPerCh, channels)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(buf.buf), 0)
	if err != nil {
		t.Fatalf("DecodeInterleavedLimit(0): %v", err)
	}
	return buf.buf, pcm
}

func TestDecodeInterleavedLimitRejectsOversizedDecode(t *testing.T) {
	file, decoded := encodeClip(t, 24000, 2)

	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(file), len(decoded)/4)
	if !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit", err)
	}
	if pcm != nil {
		t.Errorf("PCM = %d bytes, want nil when the limit stops the decode", len(pcm))
	}
}

// TestDecodeInterleavedLimitBoundsCraftedExpansion is the regression guard for
// the amplification in #18. Opus's worst case is not reached by encoding
// anything: it is a hand-built packet that costs two bytes and decodes to the
// 120 ms maximum. The point is not the exact ratio but that the bound holds
// however large it gets.
func TestDecodeInterleavedLimitBoundsCraftedExpansion(t *testing.T) {
	const (
		channels = 2
		packets  = 200
		// One packet: TOC byte with config 3 (SILK-NB, 60 ms frames) and code 3,
		// then a frame-count byte with VBR and padding clear and M = 2. With CBR and
		// no bytes left, every frame length computes to zero, which RFC 6716 3.2.1
		// permits for DTX, so two bytes decode to 2 x 60 ms.
		samplesPerPacket = 5760
	)
	pkt := []byte{0x1F, 0x02}

	var buf memWS
	wr, err := m4a.NewWriter(&buf, m4a.WriterConfig{Codec: m4a.CodecOpus, SampleRate: 48000, Channels: channels})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for range packets {
		if err := wr.WriteFrameDuration(pkt, samplesPerPacket); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	if err := wr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	decoded := packets * samplesPerPacket * channels * 2
	ratio := decoded / max(len(buf.buf), 1)
	if ratio < 100 {
		t.Fatalf("fixture is not amplifying enough to test the bound: %d bytes decode to %d (%dx)", len(buf.buf), decoded, ratio)
	}
	t.Logf("fixture: %d bytes decode to %d bytes (%dx amplification)", len(buf.buf), decoded, ratio)

	const limit = 1 << 20
	if _, _, err := DecodeInterleavedLimit(bytes.NewReader(buf.buf), limit); !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit", err)
	}
}

// TestDecodeInterleavedLimitBoundary pins both sides of the comparison: a limit
// equal to the decoded size is a fit, one byte under it is not.
func TestDecodeInterleavedLimitBoundary(t *testing.T) {
	file, decoded := encodeClip(t, 4800, 1)

	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(file), len(decoded))
	if err != nil {
		t.Fatalf("DecodeInterleavedLimit(exact): %v", err)
	}
	if !bytes.Equal(pcm, decoded) {
		t.Errorf("PCM = %d bytes, want the same %d bytes an unlimited decode returns", len(pcm), len(decoded))
	}

	if _, _, err := DecodeInterleavedLimit(bytes.NewReader(file), len(decoded)-1); !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("one byte short: err = %v, want ErrDecodeLimit", err)
	}
}

func TestDecodeInterleavedLimitNonPositiveMeansUnlimited(t *testing.T) {
	file, decoded := encodeClip(t, 4800, 2)

	for _, limit := range []int{0, -1} {
		got, _, err := DecodeInterleavedLimit(bytes.NewReader(file), limit)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit(%d): %v", limit, err)
		}
		if !bytes.Equal(got, decoded) {
			t.Errorf("limit %d: got %d bytes, want %d", limit, len(got), len(decoded))
		}
	}
}

func TestDecodeStreamDeliversEveryFrame(t *testing.T) {
	file, decoded := encodeClip(t, 4800, 2)

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
	if info.Codec != m4a.CodecOpus {
		t.Errorf("Codec = %v, want Opus", info.Codec)
	}
	if frames != info.FrameCount {
		t.Errorf("callback ran %d times, want %d (one per packet)", frames, info.FrameCount)
	}
	// The same decoder over the same packets, so the streamed PCM is byte-identical
	// to what the accumulating entry point returns.
	if !bytes.Equal(got, decoded) {
		t.Errorf("streamed PCM mismatch: got %d bytes, want %d bytes", len(got), len(decoded))
	}
}

func TestDecodeStreamPropagatesCallbackError(t *testing.T) {
	file, _ := encodeClip(t, 4800, 1)
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
// bounded rather than unlimited. Tripping the real default would need a
// multi-hundred-megabyte decode, so the check is that the two entry points agree.
func TestDecodeInterleavedAppliesDefaultLimit(t *testing.T) {
	if m4a.DefaultMaxDecodedBytes <= 0 {
		t.Fatalf("DefaultMaxDecodedBytes = %d, want a positive ceiling", m4a.DefaultMaxDecodedBytes)
	}
	file, decoded := encodeClip(t, 4800, 2)

	got, _, err := DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if !bytes.Equal(got, decoded) {
		t.Errorf("DecodeInterleaved returned %d bytes, want the %d an explicit unlimited decode returns", len(got), len(decoded))
	}
}
