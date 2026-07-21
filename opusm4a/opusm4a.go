// SPDX-License-Identifier: MIT

// Package opusm4a is an optional convenience bridge that couples go-opus's Opus
// codec to the go-m4a container. EncodeInterleaved takes interleaved 16-bit PCM
// straight to a gapless Opus .mp4; DecodeInterleaved opens one and returns the
// decoded PCM. It is a thin seam over the two libraries: the core m4a package
// stays stdlib-only, and only this subpackage imports go-opus, so a consumer that
// merely muxes or demuxes never pulls the codec.
package opusm4a

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/tphakala/go-opus/opus"

	m4a "github.com/tphakala/go-m4a"
)

// opusRate is the sample rate the Opus-in-ISOBMFF encapsulation fixes for the
// container timescale, and the rate this bridge encodes and decodes at.
const opusRate = 48000

// frameSamplesPerChannel is the per-channel sample count of one encoded packet: a
// 20 ms frame at 48 kHz, the standard Opus-in-MP4 packetization.
const frameSamplesPerChannel = opusRate / 50 // 960

// maxSamplesPerChannel is the most one packet can decode to: RFC 6716 section 3.1
// caps a packet at 120 ms of audio, which is 5760 samples at 48 kHz. It sizes the
// decoder's output buffer, so it is a correctness bound rather than a hint.
const maxSamplesPerChannel = 120 * opusRate / 1000 // 5760

// maxPCMReservation is the ceiling on what an accumulating decode reserves up
// front, the same 64 MiB flacm4a uses and for the same reason: the reservation is
// derived from a file's own description, so it needs a bound that does not scale
// with what the file claims. It bounds the RESERVATION only. What bounds the
// decode is the caller's limit, which pcmReservation also applies here. See the
// note on flacm4a.maxPCMReservation for how the value was chosen.
const maxPCMReservation = 64 << 20

// Config configures Opus encoding. SampleRate must be 48000 (the Opus container
// rate); Channels is 1 or 2; Bitrate is the target bits per second (0 selects the
// go-opus automatic rate).
type Config struct {
	SampleRate int
	Channels   int
	Bitrate    int
}

// EncodeInterleaved encodes interleaved little-endian 16-bit PCM as a gapless Opus
// .mp4 to w. It splits the input into 20 ms frames (padding the final frame with
// silence), encodes each to one Opus packet, and writes an edit list that trims
// the encoder pre-skip and the trailing padding so a compliant player presents
// exactly the original audio.
func EncodeInterleaved(w io.WriteSeeker, cfg Config, pcm []byte) error {
	if cfg.SampleRate != opusRate {
		return fmt.Errorf("go-m4a/opusm4a: SampleRate %d unsupported, want %d", cfg.SampleRate, opusRate)
	}
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return fmt.Errorf("go-m4a/opusm4a: channels %d out of range, want 1 or 2", cfg.Channels)
	}
	stride := 2 * cfg.Channels // bytes per interleaved sample (S16)
	if len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/opusm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}
	samplesPerChannel := len(pcm) / stride

	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		Bitrate:    cfg.Bitrate,
	})
	if err != nil {
		return fmt.Errorf("go-m4a/opusm4a: new encoder: %w", err)
	}
	preSkip := enc.PreSkip()

	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		Codec:               m4a.CodecOpus,
		SampleRate:          opusRate,
		Channels:            cfg.Channels,
		OpusPreSkip:         preSkip,
		OpusInputSampleRate: opusRate,
		MediaLength:         int64(samplesPerChannel),
	})
	if err != nil {
		return err
	}

	// The encoder has a preSkip-sample algorithmic delay, so the last preSkip
	// content samples only emerge after that many more input samples. Encode
	// samplesPerChannel+preSkip samples (zero-padding past the input) so every
	// content sample is flushed out; the edit list trims the leading pre-skip and
	// the trailing padding.
	total := samplesPerChannel + preSkip
	frameLen := frameSamplesPerChannel * cfg.Channels // int16 values per frame
	frame := make([]int16, frameLen)
	pkt := make([]byte, 1276) // holds any single VBR Opus packet
	for off := 0; off < total; off += frameSamplesPerChannel {
		// Fill this frame's interleaved samples into the reused int16 buffer. A full
		// 20 ms frame (the common case) overwrites the whole buffer, so only a short
		// final frame and the trailing flush frames need the tail zeroed for padding.
		remaining := max(samplesPerChannel-off, 0)
		n := min(frameSamplesPerChannel, remaining) * cfg.Channels
		base := off * stride
		for i := 0; i < n; i++ {
			frame[i] = int16(binary.LittleEndian.Uint16(pcm[base+2*i:]))
		}
		for i := n; i < len(frame); i++ {
			frame[i] = 0
		}
		m, err := enc.Encode(frame, pkt)
		if err != nil {
			return fmt.Errorf("go-m4a/opusm4a: encode frame at sample %d: %w", off, err)
		}
		dur, err := opus.PacketDuration(pkt[:m])
		if err != nil {
			return fmt.Errorf("go-m4a/opusm4a: packet duration: %w", err)
		}
		if err := wr.WriteFrameDuration(pkt[:m], uint32(dur)); err != nil {
			return err
		}
	}
	return wr.Close()
}

// DecodeInterleaved opens an Opus .mp4 and decodes it to interleaved little-endian
// 16-bit PCM at 48 kHz, returning the PCM together with the container Info. The
// returned PCM includes the leading pre-skip priming and the trailing final-frame
// padding: for sample-accurate output, skip Info.EncoderDelay leading samples per
// channel, then keep Info.Duration-worth of samples (Duration times 48000 per
// channel) and discard the rest.
//
// The decode is bounded at m4a.DefaultMaxDecodedBytes: a stream that decodes to
// more stops with an error wrapping m4a.ErrDecodeLimit instead of growing the
// buffer to fit. That bound is what makes this safe to point at a file the caller
// did not produce, because a packet's decoded size is not proportional to its
// encoded size: a two-byte packet of zero-length DTX frames decodes to the 120 ms
// maximum. Use DecodeInterleavedLimit to choose the ceiling, or DecodeStream to
// decode a stream of any length without accumulating it.
func DecodeInterleaved(r io.ReadSeeker) ([]byte, m4a.Info, error) {
	return DecodeInterleavedLimit(r, m4a.DefaultMaxDecodedBytes)
}

// DecodeInterleavedLimit is DecodeInterleaved with an explicit ceiling on the
// decoded size, returning an error wrapping m4a.ErrDecodeLimit as soon as the
// audio decodes past it. A maxBytes of zero or less means no limit, which
// restores the unbounded behaviour and is for input the caller produced or
// otherwise trusts.
func DecodeInterleavedLimit(r io.ReadSeeker, maxBytes int) ([]byte, m4a.Info, error) {
	rd, dec, info, err := openStream(r)
	if err != nil {
		return nil, info, err
	}

	out := make([]byte, 0, pcmReservation(info.FrameCount, info.Channels, maxBytes))
	err = forEachPacket(rd, dec, info.Channels, func(pcm []byte) error {
		// Written as a subtraction so the test cannot overflow int on a 32-bit
		// build. len(out) never exceeds maxBytes, so the difference is non-negative,
		// and a packet that lands exactly on the limit is a fit rather than an excess.
		if maxBytes > 0 && len(pcm) > maxBytes-len(out) {
			return fmt.Errorf("go-m4a/opusm4a: decoded output exceeds the %d-byte limit: %w", maxBytes, m4a.ErrDecodeLimit)
		}
		out = append(out, pcm...)
		return nil
	})
	if err != nil {
		return nil, info, err
	}
	return out, info, nil
}

// DecodeStream opens an Opus .mp4 and decodes it one packet at a time, handing
// each packet's interleaved little-endian 16-bit PCM to fn. It accumulates
// nothing, so it decodes a stream of any length in memory proportional to a single
// packet, which is what makes it the shape to reach for with input the caller did
// not produce. The PCM carries the same priming and padding DecodeInterleaved
// returns, so the same trimming applies.
//
// The slice handed to fn aliases a buffer reused across packets and is valid only
// until fn returns; fn copies whatever it needs to keep. An error from fn stops
// the decode and is returned as-is, so a caller can break out early on its own
// sentinel.
func DecodeStream(r io.ReadSeeker, fn func(pcm []byte) error) (m4a.Info, error) {
	rd, dec, info, err := openStream(r)
	if err != nil {
		return info, err
	}
	return info, forEachPacket(rd, dec, info.Channels, fn)
}

// openStream opens r as an Opus .mp4 and builds the decoder for its channel
// layout. It is the shared prologue of the decode entry points.
func openStream(r io.ReadSeeker) (*m4a.Reader, *opus.Decoder, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, nil, m4a.Info{}, err
	}
	info := rd.Info()
	if info.Codec != m4a.CodecOpus {
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: track codec is %v, not Opus", info.Codec)
	}
	if info.Channels < 1 || info.Channels > 2 {
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: unsupported channel count %d", info.Channels)
	}
	dec, err := opus.NewDecoder(opusRate, info.Channels)
	if err != nil {
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: new decoder: %w", err)
	}
	return rd, dec, info, nil
}

// forEachPacket decodes every remaining access unit and hands the PCM to fn.
//
// Three buffers are reused for the whole stream: the access unit, the decoder's
// int16 output, and the little-endian bytes fn sees. The access-unit buffer grows
// only when a packet needs more room, which ReadFrameInto reports without
// consuming the packet, so the retry reads the same one; that is one allocation
// for the stream rather than the one per packet ReadFrame costs. The byte buffer
// replaces the two-bytes-at-a-time append this loop used to grow from nil.
func forEachPacket(rd *m4a.Reader, dec *opus.Decoder, channels int, fn func(pcm []byte) error) error {
	var au []byte
	samples := make([]int16, maxSamplesPerChannel*channels)
	pcm := make([]byte, len(samples)*2)
	for {
		n, err := rd.ReadFrameInto(au)
		if errors.Is(err, io.ErrShortBuffer) {
			au = make([]byte, n)
			n, err = rd.ReadFrameInto(au)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		got, err := dec.Decode(au[:n], samples)
		if err != nil {
			return fmt.Errorf("go-m4a/opusm4a: decode packet: %w", err)
		}
		// Defensive: the decoder cannot return more than the buffer it was given, so
		// this never fires today. It costs one comparison per packet and turns a
		// future upstream change from a panic into an error.
		if got < 0 || got*channels > len(samples) {
			return fmt.Errorf("go-m4a/opusm4a: decoder returned %d samples per channel, more than the %d-sample buffer", got, maxSamplesPerChannel)
		}
		for i := range got * channels {
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(samples[i]))
		}
		if err := fn(pcm[:got*channels*2]); err != nil {
			return err
		}
	}
}

// pcmReservation returns the byte capacity to reserve for a decoded stream of
// frameCount packets. It mirrors flacm4a.pcmReservation: believe the container
// only as far as it has paid for, then pin the result under a fixed ceiling and
// under the caller's limit.
//
// The per-packet estimate is the 20 ms packetization this bridge writes and
// ffmpeg emits, not the 120 ms a packet may hold, because a reservation is a
// guess that costs memory when it is high and a regrow when it is low: the
// standard case should be exact rather than six times too large. A stream of
// longer packets simply grows past it.
//
// Overflow safety is by construction: frameCount is pinned to the ceiling before
// it is multiplied and the running total is pinned again after, so the largest
// product is the ceiling against 960, comfortably inside uint64, and the result is
// under maxPCMReservation before the int conversion, so it fits a 32-bit int too.
func pcmReservation(frameCount, channels, limit int) int {
	if frameCount <= 0 || channels <= 0 {
		return 0
	}
	samples := min(uint64(frameCount), maxPCMReservation) * frameSamplesPerChannel
	n := min(samples*uint64(min(channels, 2)), maxPCMReservation)
	n = min(n*2, maxPCMReservation) // 16-bit samples
	if limit > 0 {
		n = min(n, uint64(limit))
	}
	return int(n)
}
