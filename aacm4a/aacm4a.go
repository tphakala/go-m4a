// SPDX-License-Identifier: MIT

// Package aacm4a is an optional convenience bridge that couples go-aac's AAC-LC
// codec to the go-m4a container. EncodeInterleaved takes interleaved PCM straight
// to a gapless .m4a; NewDecoder opens an .m4a and hands back a go-aac decoder
// that streams PCM. It is a thin seam over the two libraries: the core m4a
// package stays stdlib-only, and only this subpackage imports go-aac, so a
// consumer that merely muxes or demuxes never pulls the codec.
package aacm4a

import (
	"bytes"
	"fmt"
	"io"
	"math"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// audioObjectTypeAACLC is the MPEG-4 Audio Object Type for AAC-LC, the first
// five bits of an AudioSpecificConfig.
const audioObjectTypeAACLC = 2

// adtsHeaderLen is the length in bytes of a CRC-less ADTS frame header. go-aac
// emits ADTS without the optional CRC, so every frame is a 7-byte header
// followed immediately by the raw AAC-LC access unit.
const adtsHeaderLen = 7

// aacSampleRates is the MPEG-4 AudioSpecificConfig samplingFrequencyIndex table
// (ISO/IEC 14496-3 Table 1.16). The index of a rate in this table is its 4-bit
// samplingFrequencyIndex. It matches go-aac's asc.go and the core writer's own
// table.
var aacSampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

// EncodeInterleaved encodes interleaved little-endian signed integer PCM as an
// AAC-LC .m4a to w, with an edit list that trims the encoder priming so the file
// is sample-accurate and gapless. cfg is the same go-aac pcm.Config used for
// ADTS encoding (SampleRate 44100/48000, BitDepth 16/24/32, Channels 1/2).
func EncodeInterleaved(w io.WriteSeeker, cfg aacpcm.Config, pcm []byte) error {
	// Reject a partial trailing sample up front, with this package's prefix, so
	// the message names the container bridge rather than the encoder. A zero or
	// negative stride means cfg itself is invalid; leave that diagnosis to the
	// encoder below, which owns the canonical config error messages.
	stride := cfg.Channels * (cfg.BitDepth / 8)
	if stride > 0 && len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/aacm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}

	// Encode the whole clip to an in-memory ADTS stream. This validates cfg (a
	// bad bit depth, rate, or channel count surfaces here) and produces a
	// self-framing stream, all before anything is written to w.
	var adts bytes.Buffer
	// Reserve the whole stream up front. Growing from empty doubles its way to the
	// final size, so a clip that ends up around 180 KB allocates every intermediate
	// buffer on the way and copies each one, for several hundred KB of pure churn.
	// The final size is predictable from the config, so one reservation replaces the
	// chain. The stride guard is load-bearing, not decorative: it is what keeps the
	// division below from dividing by zero on a config the encoder has not validated
	// yet. estimateADTSSize checks every other field itself and returns 0 for
	// anything it cannot size, so an invalid config still reaches the encoder and
	// comes back as the same error it always did. The n > 0 guard is the last line
	// of defense for this call site: Grow panics on a negative count, and gating on
	// a positive one means no future regression inside the estimate can ever turn
	// a config error into a crash here.
	if stride > 0 {
		if n := estimateADTSSize(cfg, len(pcm)/stride); n > 0 {
			adts.Grow(n)
		}
	}
	if err := aacpcm.EncodeInterleaved(&adts, cfg, pcm); err != nil {
		return err
	}
	// aacpcm.EncodeInterleaved validated cfg above, so stride is positive here.
	// Guard explicitly anyway so the division can never panic if go-aac's
	// validation ever stops rejecting a zero channel count or sub-byte depth.
	if stride <= 0 {
		return fmt.Errorf("go-m4a/aacm4a: invalid stride for %d channels at %d-bit PCM", cfg.Channels, cfg.BitDepth)
	}
	samplesPerChannel := len(pcm) / stride

	asc, err := audioSpecificConfig(cfg.SampleRate, cfg.Channels)
	if err != nil {
		return err
	}

	// EncoderDelay comes straight from the codec (aac.EncoderDelay, go-aac issue
	// #27) rather than go-m4a's default literal, so the edit-list priming trim
	// tracks go-aac automatically if the codec's framing ever changes. MediaLength
	// pins the edit-list segment to the exact source length so trailing padding
	// is excluded too.
	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		SampleRate:   cfg.SampleRate,
		Channels:     cfg.Channels,
		ASC:          asc,
		EncoderDelay: aac.EncoderDelay,
		MediaLength:  int64(samplesPerChannel),
	})
	if err != nil {
		return err
	}

	if err := writeADTSFrames(wr, adts.Bytes()); err != nil {
		return err
	}
	return wr.Close()
}

// Constants behind the ADTS size estimate.
const (
	// defaultBitrate is the total bitrate go-aac's pcm.Config selects when Bitrate
	// is left at zero (FFmpeg's default of 200 kb/s). go-aac keeps this value
	// unexported, so unlike aac.FrameSize it has to be restated here.
	defaultBitrate = 200000
	// maxFrameBytesPerChannel is the AAC bit-reservoir ceiling of 6144 bits per
	// channel per frame (ISO/IEC 14496-3), in bytes. For the mono and stereo
	// AAC-LC this package encodes, no conforming frame exceeds it, which makes it
	// a sound upper bound on the encoded size.
	maxFrameBytesPerChannel = 6144 / 8
	// maxChannels is the channel count go-aac supports. The estimate refuses to
	// guess beyond it, which also keeps the arithmetic below in range.
	maxChannels = 2
	// minFrameBytesPerChannel is the measured floor cost of one encoded frame:
	// scalefactor and section data keep a go-aac frame from shrinking below
	// roughly this many payload bytes per channel however low the target bitrate,
	// so at 16 or 32 kb/s the real output runs well above nominal (by up to 2.4x
	// on the sweep behind estimate_test.go). Taking the larger of the nominal
	// payload and this floor is what keeps low-bitrate reservations honest
	// without inflating mainstream ones, where the nominal term dominates and the
	// floor never engages.
	minFrameBytesPerChannel = 56
	// bitrateMarginDivisor gives the estimate a 1/16 (6.25%) margin over the
	// nominal payload. It covers the ABR overshoot measured at mainstream
	// bitrates (up to about 3%); the far larger overshoot at very low bitrates
	// is what minFrameBytesPerChannel handles, not this margin, and any residual
	// undershoot costs exactly one buffer regrow rather than correctness.
	bitrateMarginDivisor = 16
)

// estimateADTSSize predicts the encoded ADTS stream length in bytes for cfg and a
// clip of samplesPerChannel samples: the bitrate over the coded duration, floored
// at the per-frame minimum the encoder cannot go below, plus one 7-byte header
// per frame, plus a small margin. The result is clamped to the AAC bit-reservoir
// ceiling so an unreasonably high Bitrate (which go-aac clamps rather than
// rejects) cannot turn into an unreasonably large reservation.
//
// It is only a capacity hint, so being off costs nothing but a reallocation.
// Measured across the sweep in estimate_test.go, the reservation covers the real
// stream for every clip of at least a second at every supported bitrate, and is
// never below half of it, so the worst case anywhere is the single regrow that
// bytes.Buffer's doubling gives; chasing the last short-clip corner instead
// would inflate every mainstream encode.
//
// The function runs before go-aac has validated cfg, so no field can be trusted.
// It has been broken twice by an argument that some particular product or sum
// could not overflow, so it no longer rests on that kind of argument at all: sat
// pins the raw inputs and every named intermediate into 0..maxReservation, both
// bounds, and each arithmetic expression combines at most two pinned values (or
// a small constant). A product of two values at most MaxInt32 fits int64 as a
// matter of type width, so there is no input, in range or wildly out of it, for
// which any step here can wrap. The result is therefore always in 0..MaxInt32:
// never negative, which matters because bytes.Buffer.Grow panics on a negative,
// and always expressible as an int on a 32-bit build.
func estimateADTSSize(cfg aacpcm.Config, samplesPerChannel int) int {
	if samplesPerChannel <= 0 || cfg.SampleRate <= 0 ||
		cfg.Channels <= 0 || cfg.Channels > maxChannels {
		return 0
	}

	// maxReservation bounds the return value, so pinning every intermediate to it
	// cannot change the answer for any input that yields a usable reservation;
	// the buffer still grows on demand past it.
	const maxReservation = math.MaxInt32
	sat := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		if v > maxReservation {
			return maxReservation
		}
		return v
	}

	// Pin the two unguarded-magnitude inputs before any arithmetic touches them.
	// The int64 conversions are exact, so this is the last point where an
	// out-of-range value exists at all. A clip longer than maxReservation samples
	// reserves the same as one exactly that long, which under-reserves only for
	// inputs past 2 GiB of samples per channel.
	samples := sat(int64(samplesPerChannel))
	rate := sat(int64(cfg.SampleRate))

	// The encoder codes whole frames of the clip plus aac.EncoderDelay priming
	// samples; this round-up reproduces go-aac's emitted frame count exactly.
	// Basing the payload on the coded length rather than the raw sample count is
	// what keeps short clips honest: a 20 ms clip still costs two full frames.
	frames := sat((samples + aac.EncoderDelay + aac.FrameSize - 1) / aac.FrameSize)
	codedSamples := sat(frames * aac.FrameSize)

	bitrate := int64(cfg.Bitrate)
	if bitrate <= 0 {
		bitrate = defaultBitrate
	}
	// Dividing first cannot overflow whatever Bitrate holds, and sat pins the
	// quotient, so the payload product multiplies two pinned values.
	bytesPerSecond := sat(bitrate / 8)
	payload := sat(bytesPerSecond * codedSamples / rate)
	// Channels is guarded to 1..maxChannels, so the floor is a small constant
	// times a pinned value.
	floorPayload := sat(minFrameBytesPerChannel * int64(cfg.Channels) * frames)
	if payload < floorPayload {
		payload = floorPayload
	}
	payload = sat(payload + payload/bitrateMarginDivisor)
	estimate := sat(payload + adtsHeaderLen*frames)

	ceiling := sat(frames * (maxFrameBytesPerChannel*int64(cfg.Channels) + adtsHeaderLen))
	if estimate > ceiling {
		estimate = ceiling
	}
	return int(estimate)
}

// audioSpecificConfig builds the 2-byte AAC-LC AudioSpecificConfig for the given
// rate and channel count, matching go-aac's asc.go: 5-bit audio object type (2),
// 4-bit samplingFrequencyIndex, 4-bit channelConfiguration, then three zero
// GASpecificConfig bits. For 48000 mono this is {0x11, 0x88}.
func audioSpecificConfig(sampleRate, channels int) ([]byte, error) {
	srIndex := -1
	for i, r := range aacSampleRates {
		if r == sampleRate {
			srIndex = i
			break
		}
	}
	if srIndex < 0 {
		return nil, fmt.Errorf("go-m4a/aacm4a: sample rate %d Hz has no AudioSpecificConfig index", sampleRate)
	}
	v := (audioObjectTypeAACLC << 11) | (srIndex << 7) | (channels << 3)
	return []byte{byte(v >> 8), byte(v)}, nil
}

// writeADTSFrames splits a CRC-less ADTS buffer into frames and writes each
// frame's raw access unit to wr. The ADTS stream comes from go-aac, so a
// malformed frame is an internal invariant break rather than untrusted input;
// every field is still bounds-checked and any inconsistency is returned wrapped.
func writeADTSFrames(wr *m4a.Writer, adts []byte) error {
	for i := 0; i < len(adts); {
		if i+adtsHeaderLen > len(adts) {
			return fmt.Errorf("go-m4a/aacm4a: internal error: truncated ADTS header at offset %d of %d bytes", i, len(adts))
		}
		// Syncword: byte 0 is 0xFF, and the top nibble plus the two layer bits of
		// byte 1 must be 1111x00x, i.e. (byte1 & 0xF6) == 0xF0.
		if adts[i] != 0xff || adts[i+1]&0xf6 != 0xf0 {
			return fmt.Errorf("go-m4a/aacm4a: internal error: no ADTS syncword at offset %d", i)
		}
		// protection_absent (byte 1, bit 0) must be 1: go-aac emits CRC-less
		// frames, so the header is exactly 7 bytes. A 0 here would mean a 9-byte
		// CRC header, invalidating the fixed payload offset below.
		if adts[i+1]&0x01 != 0x01 {
			return fmt.Errorf("go-m4a/aacm4a: internal error: CRC-protected ADTS frame at offset %d", i)
		}
		frameLen := (int(adts[i+3]&0x03) << 11) | (int(adts[i+4]) << 3) | (int(adts[i+5]) >> 5)
		if frameLen < adtsHeaderLen || i+frameLen > len(adts) {
			return fmt.Errorf("go-m4a/aacm4a: internal error: ADTS frame length %d at offset %d overruns %d-byte buffer", frameLen, i, len(adts))
		}
		if err := wr.WriteFrame(adts[i+adtsHeaderLen : i+frameLen]); err != nil {
			return err
		}
		i += frameLen
	}
	return nil
}

// NewDecoder opens an MP4/M4A file and returns a go-aac decoder that streams
// interleaved little-endian S16 PCM, together with the container Info
// (SampleRate, Channels, FrameCount, EncoderDelay, Duration). The go-aac decoder
// is not edit-list aware: it emits every decoded sample (FrameCount*1024 per
// channel), including both the leading priming and the trailing final-frame
// padding. For sample-accurate output, skip Info.EncoderDelay leading samples
// per channel, then keep only as many samples per channel as Info.Duration
// represents (Duration*SampleRate), discarding the trailing padding the edit
// list excludes.
//
// This package has no counterpart to flacm4a.DecodeInterleaved or
// opusm4a.DecodeInterleaved, so it has no counterpart to their
// m4a.DefaultMaxDecodedBytes ceiling either. Nothing here accumulates: the
// decoder streams, and a caller that accumulates its output itself (io.ReadAll
// rather than io.Copy to a sink) owns that bound. Size it against the audio
// rather than against the file, because the decoded size is not proportional to
// the input here either: a near-minimal AAC-LC access unit still decodes to a
// fixed 1024 samples per channel.
func NewDecoder(r io.ReadSeeker) (*aacpcm.Decoder, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, m4a.Info{}, err
	}
	info := rd.Info()
	d, err := aacpcm.NewDecoder(rd.RawStream(), aacpcm.WithRawStream(info.ASC))
	if err != nil {
		return nil, info, err
	}
	return d, info, nil
}
