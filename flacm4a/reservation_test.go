// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"testing"

	flacpcm "github.com/tphakala/go-flac/pcm"
	"github.com/tphakala/go-m4a/internal/reservation"

	m4a "github.com/tphakala/go-m4a"
)

// genPCM builds interleaved little-endian PCM at the given bit depth, extending
// genS16 to the 24-bit case the reservation has to size correctly too. Any other
// depth is a mistake in the caller rather than something to guess at.
func genPCM(t *testing.T, samplesPerCh, channels, bitDepth int) []byte {
	t.Helper()
	switch bitDepth {
	case 16:
		return genS16(samplesPerCh, channels)
	case 24:
		out := make([]byte, 0, samplesPerCh*channels*3)
		for i := range samplesPerCh {
			for c := range channels {
				v := uint32(int32(math.Round(4000000 * math.Sin(float64(i)*(0.02+0.005*float64(c))))))
				out = append(out, byte(v), byte(v>>8), byte(v>>16))
			}
		}
		return out
	default:
		t.Fatalf("genPCM: unsupported bit depth %d, want 16 or 24", bitDepth)
		return nil
	}
}

func TestPCMReservation(t *testing.T) {
	t.Parallel()
	// framesFor is a frame count large enough not to bind the reservation, for
	// the rows that are exercising the declared-length arithmetic instead.
	const framesFor = 1000
	// seAbsent is a sample entry that states no channel count, so the reservation
	// falls back to STREAMINFO's. Used by every row not exercising the two-source
	// comparison itself.
	const seAbsent = 0
	cases := []struct {
		name         string
		totalSamples uint64
		frameCount   int
		siChannels   int
		seChannels   int
		bitDepth     int
		want         int
	}{
		{"unknown length", 0, framesFor, 2, seAbsent, 16, 0},
		{"no frames in the container", 48000, 0, 2, seAbsent, 16, 0},
		{"negative frame count", 48000, -1, 2, seAbsent, 16, 0},
		{"zero channels", 1000, framesFor, 0, seAbsent, 16, 0},
		{"zero bit depth", 1000, framesFor, 2, seAbsent, 0, 0},
		{"negative channels", 1000, framesFor, -2, seAbsent, 16, 0},
		{"negative bit depth", 1000, framesFor, 2, seAbsent, -16, 0},
		{"mono 16-bit", 48000, framesFor, 1, seAbsent, 16, 48000 * 2},
		{"stereo 16-bit", 48000, framesFor, 2, seAbsent, 16, 48000 * 4},
		{"stereo 24-bit", 48000, framesFor, 2, seAbsent, 24, 48000 * 6},
		{"stereo 8-bit", 48000, framesFor, 2, seAbsent, 8, 48000 * 2},
		// A depth that is not a whole number of bytes rounds up, matching the
		// stride the decoder writes.
		{"12-bit rounds to 2 bytes", 1000, framesFor, 1, seAbsent, 12, 2000},
		// The sample entry narrows the reservation when it states fewer channels,
		// but only when it states something: nothing validates that field, so a
		// zero or negative there means absent, not none, and must not disable the
		// reservation.
		{"sample entry narrows the channel count", 48000, framesFor, 8, 2, 16, 48000 * 2 * 2},
		{"sample entry is ignored when larger", 48000, framesFor, 2, 8, 16, 48000 * 2 * 2},
		{"absent sample entry count does not disable", 48000, framesFor, 2, 0, 16, 48000 * 2 * 2},
		{"negative sample entry count does not disable", 48000, framesFor, 2, -1, 16, 48000 * 2 * 2},
		// The container bound is what stops a file inflating its own reservation.
		// One access unit can hold at most maxFLACBlockSize samples, however many
		// the header claims.
		{"one frame bounds an absurd claim", math.MaxUint64, 1, 1, seAbsent, 16, maxFLACBlockSize * 2},
		{"two frames bound an absurd claim", math.MaxUint64, 2, 2, seAbsent, 16, maxFLACBlockSize * 2 * 4},
		// Everything below is a file lying about its size while carrying enough
		// frames that the container bound does not bind. None may exceed the cap,
		// and none may overflow.
		{"absurd sample count", math.MaxUint64, math.MaxInt, 2, seAbsent, 16, reservation.MaxPCMReservation},
		// A lie about channels or depth is clamped to the FLAC maximum rather
		// than to the cap, so a small declared sample count still reserves small.
		{"absurd channels", 48000, framesFor, math.MaxInt, seAbsent, 16, 48000 * 8 * 2},
		{"absurd bit depth", 48000, framesFor, 2, seAbsent, math.MaxInt, 48000 * 2 * 4},
		{"absurd everything", math.MaxUint64, math.MaxInt, math.MaxInt, math.MaxInt, math.MaxInt, reservation.MaxPCMReservation},
		{"exactly at the cap", reservation.MaxPCMReservation / 4, framesFor, 2, seAbsent, 16, reservation.MaxPCMReservation},
		{"one sample under the cap", reservation.MaxPCMReservation/4 - 1, framesFor, 2, seAbsent, 16, reservation.MaxPCMReservation - 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// These cases pin the reservation the container and STREAMINFO imply, so
			// they run with no caller limit; TestPCMReservationRespectsLimit covers
			// what a limit does to the same arithmetic.
			const noLimit = 0
			got := pcmReservation(tc.totalSamples, tc.frameCount, tc.siChannels, tc.seChannels, tc.bitDepth, noLimit)
			if got != tc.want {
				t.Errorf("pcmReservation(%d, %d, %d, %d, %d) = %d, want %d",
					tc.totalSamples, tc.frameCount, tc.siChannels, tc.seChannels, tc.bitDepth, got, tc.want)
			}
			if got < 0 || got > reservation.MaxPCMReservation {
				t.Errorf("reservation %d escaped 0..%d", got, reservation.MaxPCMReservation)
			}
		})
	}
}

// The trim policy itself (ShouldTrim) is tested in internal/reservation, shared
// with opusm4a. What stays here is the FLAC-specific reservation arithmetic and
// the end-to-end assertion that a real decode does not over-retain.

// TestPCMReservationExact checks the reservation matches the decoded length for
// a real round trip, so DecodeInterleaved neither regrows nor over-reserves on
// the streams this bridge produces. It is also the package's only 24-bit round
// trip, so it asserts the bytes as well as the shape.
func TestPCMReservationExact(t *testing.T) {
	t.Parallel()
	for _, channels := range []int{1, 2} {
		for _, bitDepth := range []int{16, 24} {
			t.Run(fmt.Sprintf("%dch_%dbit", channels, bitDepth), func(t *testing.T) {
				t.Parallel()
				pcm := genPCM(t, 10000, channels, bitDepth)
				cfg := Config{SampleRate: 48000, Channels: channels, BitDepth: bitDepth, CompressionLevel: 5}
				w := &memWS{}
				if err := EncodeInterleaved(w, cfg, pcm); err != nil {
					t.Fatalf("encode: %v", err)
				}
				out, _, err := DecodeInterleaved(bytes.NewReader(w.buf))
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !bytes.Equal(out, pcm) {
					t.Errorf("round trip is not bit-exact: decoded %d bytes, want %d", len(out), len(pcm))
				}
				if cap(out) != len(pcm) {
					t.Errorf("reserved capacity %d, want exactly %d (a regrow or an over-reserve)",
						cap(out), len(pcm))
				}
			})
		}
	}
}

// inflateDeclaredLength rewrites the 36-bit total-samples field of the STREAMINFO
// carried in the file's dfLa box to its maximum, leaving every other field and
// every audio frame untouched. STREAMINFO packs that field into the 8 bytes at
// offset 10, sharing them with sampleRate(20), channels(3) and bitsPerSample(5).
func inflateDeclaredLength(t *testing.T, enc []byte) {
	t.Helper()
	// Search from the end: go-m4a writes mdat before moov, so a forward scan runs
	// through the compressed audio first and could match a chance "dfLa" in it.
	idx := bytes.LastIndex(enc, []byte("dfLa"))
	if idx < 0 {
		t.Fatal("no dfLa box in the encoded file")
	}
	// dfLa: 4 name + 4 fullbox version/flags + 4 metadata block header, then the
	// 34-byte STREAMINFO body.
	si := idx + 12
	const streamInfoLen = 34
	if si+streamInfoLen > len(enc) {
		t.Fatalf("dfLa at %d leaves no room for STREAMINFO in %d bytes", idx, len(enc))
	}
	const totalSamplesMask = (uint64(1) << 36) - 1
	word := binary.BigEndian.Uint64(enc[si+10 : si+18])
	binary.BigEndian.PutUint64(enc[si+10:si+18], word|totalSamplesMask)
}

// TestDecodeDoesNotAmplifyOverdeclaredLength is the regression guard for the
// reservation being driven by an untrusted self-description. A file that claims
// the maximum 2^36 samples while carrying one short frame must not make the
// caller allocate, or hold, a buffer sized for the claim.
// It deliberately does not call t.Parallel, because it measures a process-wide
// TotalAlloc delta and Go runs every parallel test only after the sequential ones
// finish. The invariant that actually keeps this honest is broader than that,
// though, and worth stating: no test in this package may leak a goroutine that
// allocates. A parallel neighbour is harmless, but a leaked allocator from an
// earlier sequential test lands directly in this delta and was measured turning a
// 158 KB reading into megabytes.
func TestDecodeDoesNotAmplifyOverdeclaredLength(t *testing.T) {
	pcm := genS16(1000, 1)
	cfg := Config{SampleRate: 48000, Channels: 1, BitDepth: 16, CompressionLevel: 5}
	w := &memWS{}
	if err := EncodeInterleaved(w, cfg, pcm); err != nil {
		t.Fatalf("encode: %v", err)
	}
	enc := w.buf
	inflateDeclaredLength(t, enc)

	// Assert the precondition. inflateDeclaredLength locates the box by scanning
	// for a magic string, so a change to the box layout could leave it patching
	// nothing, and the test would then pass by testing an honest file. Confirm the
	// decoder really does see the inflated claim before measuring anything.
	rd, err := m4a.NewReader(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	fd, err := flacpcm.NewFrameDecoder(rd.Info().CodecConfig)
	if err != nil {
		t.Fatalf("frame decoder: %v", err)
	}
	const wantDeclared = uint64(1)<<36 - 1
	if got := fd.StreamInfo().TotalSamples; got != wantDeclared {
		t.Fatalf("precondition: STREAMINFO declares %d samples, want the inflated %d", got, wantDeclared)
	}

	// Measure what the decode actually allocates, not just what it hands back.
	// The trim rewrites the returned slice, so inspecting only that would report
	// success even if the reservation had believed the claim outright: this test
	// passed with the container bound deleted entirely until the allocation was
	// measured here.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, _, err := DecodeInterleaved(bytes.NewReader(enc))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc

	// The audio is untouched by the edit, so the decode must still be exact.
	if !bytes.Equal(out, pcm) {
		t.Errorf("decoded %d bytes, want the original %d", len(out), len(pcm))
	}
	// The allocation half: one short frame must not reserve for the declared 2^36.
	// The bound is absolute rather than a fraction of reservation.MaxPCMReservation, because
	// the honest figure here has nothing to do with that constant: it is measured
	// at about 158 KB (a 131 KB reservation from the one-frame container bound,
	// plus reader and decoder overhead) and does not move when the ceiling does.
	// Keying it to the ceiling would silently loosen this guard if the ceiling
	// rose, and break it on correct code if the ceiling fell far enough.
	const maxHonestDecodeAlloc = 1 << 20
	if allocated > maxHonestDecodeAlloc {
		t.Errorf("decoding a %d-byte file allocated %d bytes for %d bytes of audio; "+
			"the declared length was believed", len(enc), allocated, len(pcm))
	}
	// The retention half: the returned slice is what the caller holds on to.
	if slack := cap(out) - len(out); slack > reservation.MaxRetainedSlack {
		t.Errorf("returned slice retains %d bytes of slack for %d bytes of audio, want at most %d",
			slack, len(out), reservation.MaxRetainedSlack)
	}
}
