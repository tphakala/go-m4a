// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// encodeClip encodes a sine to an Opus .mp4 and returns the file bytes together
// with what an unlimited decode produces from it. Opus is lossy, so the decoded
// length is the reference the limit cases are stated against rather than the
// input length.
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

// muxPackets writes the given Opus packets into an .mp4 as one access unit each,
// with dur samples of duration apiece. It is how the crafted fixtures are built:
// the encoder only ever emits 20 ms packets, so anything else has to be muxed by
// hand.
func muxPackets(t *testing.T, channels, dur int, packets [][]byte) []byte {
	t.Helper()
	var buf memWS
	wr, err := m4a.NewWriter(&buf, m4a.WriterConfig{Codec: m4a.CodecOpus, SampleRate: 48000, Channels: channels})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, pkt := range packets {
		if err := wr.WriteFrameDuration(pkt, uint32(dur)); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	if err := wr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.buf
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
// 120 ms maximum. The ratio is measured from what the decoder actually produced
// rather than from the duration the fixture declared, so it also pins the claim
// that such a packet really does decode to 120 ms.
func TestDecodeInterleavedLimitBoundsCraftedExpansion(t *testing.T) {
	const (
		channels = 2
		packets  = 200
		// One packet: TOC byte with config 3 (SILK-NB, 60 ms frames), the stereo bit
		// set, and code 3, then a frame-count byte with VBR and padding clear and
		// M = 2. With CBR and no bytes left, every frame length computes to zero,
		// which RFC 6716 3.2.1 permits for DTX, so two bytes decode to 2 x 60 ms.
		samplesPerPacket = 5760
	)
	pkt := []byte{0x1F, 0x02}
	fixture := make([][]byte, packets)
	for i := range fixture {
		fixture[i] = pkt
	}
	file := muxPackets(t, channels, samplesPerPacket, fixture)

	full, _, err := DecodeInterleavedLimit(bytes.NewReader(file), 0)
	if err != nil {
		t.Fatalf("DecodeInterleavedLimit(0): %v", err)
	}
	if want := packets * samplesPerPacket * channels * 2; len(full) != want {
		t.Fatalf("decoded %d bytes, want %d: a two-byte DTX packet must decode to 120 ms", len(full), want)
	}
	ratio := len(full) / max(len(file), 1)
	if ratio < 100 {
		t.Fatalf("fixture is not amplifying enough to test the bound: %d bytes decode to %d (%dx)", len(file), len(full), ratio)
	}
	t.Logf("fixture: %d bytes decode to %d bytes (%dx amplification)", len(file), len(full), ratio)

	const limit = 1 << 20
	if _, _, err := DecodeInterleavedLimit(bytes.NewReader(file), limit); !errors.Is(err, m4a.ErrDecodeLimit) {
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

// TestPCMReservation covers the reservation arithmetic directly: it is derived
// from a count the container states, so it needs to stay inside its ceiling
// whatever that count says, and inside the caller's limit whenever there is one.
func TestPCMReservation(t *testing.T) {
	t.Parallel()
	// 20 ms stereo packets: the packetization the estimate assumes, so the
	// reservation for an honest file of them is exact.
	const oneSecond = 50
	if got, want := pcmReservation(oneSecond, 2, 0), 48000*2*2; got != want {
		t.Errorf("pcmReservation(50 packets, stereo) = %d, want %d (one second of 48 kHz stereo)", got, want)
	}

	cases := []struct {
		name                              string
		frameCount, channels, limit, want int
	}{
		{"no frames", 0, 2, 0, 0},
		{"no channels", 50, 0, 0, 0},
		{"negative frame count", -1, 2, 0, 0},
		{"absurd frame count clamps to the ceiling", math.MaxInt, 2, 0, maxPCMReservation},
		{"absurd channel count clamps to stereo", oneSecond, math.MaxInt, 0, 48000 * 2 * 2},
		{"limit below the estimate", oneSecond, 2, 4096, 4096},
		{"limit above the estimate is not a floor", oneSecond, 2, math.MaxInt, 48000 * 2 * 2},
		{"limit and ceiling together", math.MaxInt, 2, 1 << 20, 1 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pcmReservation(tc.frameCount, tc.channels, tc.limit)
			if got != tc.want {
				t.Errorf("pcmReservation(%d, %d, %d) = %d, want %d", tc.frameCount, tc.channels, tc.limit, got, tc.want)
			}
			if got < 0 || got > maxPCMReservation {
				t.Errorf("reservation %d escaped 0..%d", got, maxPCMReservation)
			}
		})
	}
}

// TestShouldTrim pins the rule that decides whether an accumulating decode hands
// back its buffer or a right-sized copy. The rows are flacm4a's, deliberately:
// the two bridges apply the same rule with the same thresholds, and the rows
// that matter are the ones that discriminate between plausible divisors. A table
// of round numbers passes for any divisor from a half to a quarter and so
// defends nothing; the "unknown length, 30s" row sits at ratio 0.2501 and is what
// rules out a quarter. See flacm4a.shouldTrim for the measurements behind them.
func TestShouldTrim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		length   int
		slack    int
		wantTrim bool
	}{
		// The case the trim exists for: far more was reserved than decoded.
		{"over-reserved", 2000, 129070, true},
		{"over-reserved at the ceiling", 2000, maxPCMReservation - 2000, true},
		// Pins the firing side closely enough that the policy cannot be loosened
		// several times over with a green suite.
		{"slack over half the audio", 1000000, 600000, true},
		// Honest streams: a buffer that grew by append carries up to about a quarter
		// of its length as headroom, and none of that is worth a copy.
		{"exact reservation", 2880000, 0, false},
		{"unknown length, 5s", 960000, 236032, false},
		{"unknown length, 30s", 5760000, 1440768, false},
		// Small absolute slack is never worth a copy whatever the proportion.
		{"tiny buffer, proportionally huge slack", 8, 1024, false},
		{"empty buffer", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldTrim(tc.length, tc.length+tc.slack); got != tc.wantTrim {
				t.Errorf("shouldTrim(len=%d, cap=%d) = %v, want %v (slack %d)",
					tc.length, tc.length+tc.slack, got, tc.wantTrim, tc.slack)
			}
		})
	}
}

// TestDecodeInterleavedTrimsOverReservation covers the case the trim exists for.
// The reservation assumes the 20 ms packetization this bridge writes, so a file
// of 120 ms packets decodes to six times what was reserved (fine, it grows), and
// one of short packets decodes to a fraction of it. Without the trim the caller
// would keep the whole reservation for as long as it kept the PCM: measured at
// 13.4 MB of slack behind 1.9 MB of PCM.
func TestDecodeInterleavedTrimsOverReservation(t *testing.T) {
	// A file whose packets decode to far less than the 20 ms the reservation
	// assumes: 2.5 ms CELT-NB frames, the shortest Opus packetization there is.
	const (
		channels = 2
		packets  = 4000
	)
	pkt := []byte{0x84, 0x00} // TOC: config 16 (CELT-NB 2.5 ms), stereo, code 0
	fixture := make([][]byte, packets)
	for i := range fixture {
		fixture[i] = pkt
	}
	file := muxPackets(t, channels, 120, fixture) // 2.5 ms at 48 kHz

	pcm, _, err := DecodeInterleavedLimit(bytes.NewReader(file), 0)
	if err != nil {
		t.Fatalf("DecodeInterleavedLimit: %v", err)
	}
	// Asserted against the threshold rather than by calling shouldTrim, so that a
	// change loosening the predicate cannot make this test agree with it.
	if slack := cap(pcm) - len(pcm); slack > maxRetainedSlack {
		t.Errorf("returned buffer keeps %d bytes of slack behind %d bytes of PCM", slack, len(pcm))
	}
}

// TestDecodeStreamRegrowsMidStream drives the io.ErrShortBuffer retry, which is
// the newest code in the decode loop and the one path a naturally-encoded fixture
// does not reach: every packet the encoder emits is about the same size, and the
// largest tends to come first, so the buffer is sized once and never grown again.
// Here each packet is strictly larger than the last, so every iteration takes the
// retry. The expected size is derived from the fixture rather than from a second
// decode, so a retry that dropped or repeated an access unit cannot agree with it.
func TestDecodeStreamRegrowsMidStream(t *testing.T) {
	const (
		channels = 2
		packets  = 24
		// 2.5 ms CELT-NB frames again, so the payload can be padded to any length
		// without changing the decoded size of a packet.
		samplesPerPacket = 120
	)
	fixture := make([][]byte, packets)
	for i := range fixture {
		// Ascending lengths: 2, 3, 4 ... bytes, all decoding to the same duration.
		pkt := make([]byte, i+2)
		pkt[0] = 0x84
		fixture[i] = pkt
	}
	file := muxPackets(t, channels, samplesPerPacket, fixture)

	frames, total := 0, 0
	info, err := DecodeStream(bytes.NewReader(file), func(pcm []byte) error {
		frames++
		total += len(pcm)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	if frames != packets {
		t.Errorf("callback ran %d times, want %d: the short-buffer retry must re-read the same access unit, not skip it", frames, packets)
	}
	if want := packets * samplesPerPacket * channels * 2; total != want {
		t.Errorf("streamed %d bytes, want %d", total, want)
	}
	if info.FrameCount != packets {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, packets)
	}
}

func TestDecodeStreamDeliversEveryFrame(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			file, decoded := encodeClip(t, 4800, channels)

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
			// Guards the assertion below from going vacuous if the fixture ever
			// shrinks to a single packet.
			if info.FrameCount < 2 {
				t.Fatalf("fixture has %d packets, want a multi-packet stream", info.FrameCount)
			}
			if frames != info.FrameCount {
				t.Errorf("callback ran %d times, want %d (one per packet)", frames, info.FrameCount)
			}
			// The same decoder over the same packets, so the streamed PCM is
			// byte-identical to what the accumulating entry point returns.
			if !bytes.Equal(got, decoded) {
				t.Errorf("streamed PCM mismatch: got %d bytes, want %d bytes", len(got), len(decoded))
			}
		})
	}
}

func TestDecodeStreamPropagatesCallbackError(t *testing.T) {
	file, _ := encodeClip(t, 4800, 1)
	stop := errors.New("caller stopped the decode")

	// Stopping partway matters as much as stopping at once: an implementation that
	// only checked the first callback's error would pass a first-packet-only test.
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
	file, _ := encodeClip(t, 4800, 1)

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
	if defaultMaxDecodedBytes != m4a.DefaultMaxDecodedBytes {
		t.Fatalf("defaultMaxDecodedBytes = %d, want the package constant %d", defaultMaxDecodedBytes, m4a.DefaultMaxDecodedBytes)
	}
	file, decoded := encodeClip(t, 24000, 2)

	restore := defaultMaxDecodedBytes
	t.Cleanup(func() { defaultMaxDecodedBytes = restore })

	defaultMaxDecodedBytes = len(decoded) / 4
	if _, _, err := DecodeInterleaved(bytes.NewReader(file)); !errors.Is(err, m4a.ErrDecodeLimit) {
		t.Fatalf("err = %v, want ErrDecodeLimit: DecodeInterleaved must delegate with the package default", err)
	}

	// And the same file decodes once the ceiling is above it, so the failure above
	// is the limit rather than anything else about the fixture.
	defaultMaxDecodedBytes = len(decoded)
	got, _, err := DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved under a sufficient default: %v", err)
	}
	if !bytes.Equal(got, decoded) {
		t.Errorf("PCM = %d bytes, want %d", len(got), len(decoded))
	}
}
