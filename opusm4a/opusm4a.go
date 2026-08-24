// SPDX-License-Identifier: MIT

// Package opusm4a is an optional convenience bridge that couples go-opus's Opus
// codec to the go-m4a container. EncodeInterleaved takes interleaved 16-bit PCM
// straight to a gapless Opus .mp4; DecodeInterleaved opens one and returns the
// decoded PCM, bounded at m4a.DefaultMaxDecodedBytes, and DecodeStream decodes a
// file of any length a packet at a time without accumulating it, which is the
// shape to reach for with input the caller did not produce. It is a thin seam
// over the two libraries: the core m4a package stays stdlib-only, and only this
// subpackage imports go-opus, so a consumer that merely muxes or demuxes never
// pulls the codec.
package opusm4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/tphakala/go-opus/opus"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/reservation"
)

// opusRate is the sample rate the Opus-in-ISOBMFF encapsulation fixes for the
// container timescale, and the rate this bridge encodes and decodes at.
const opusRate = 48000

// frameSamplesPerChannel is a 20 ms frame at 48 kHz: the frame size
// EncodeInterleaved writes, which is also the standard Opus-in-MP4
// packetization, and the per-packet estimate pcmReservation assumes on decode. A
// foreign file may use any Opus frame size from 2.5 to 120 ms, so on the decode
// side this is a guess rather than a fact.
const frameSamplesPerChannel = opusRate / 50 // 960

// maxSamplesPerChannel is the most one packet can decode to: RFC 6716 section
// 3.2.5 requires that "the audio duration contained within a packet MUST NOT
// exceed 120 ms" (rule [R5], restated in section 3.4), which is 5760 samples at
// 48 kHz. It sizes the decoder's output buffer, so it is a correctness bound
// rather than a hint.
const maxSamplesPerChannel = 120 * opusRate / 1000 // 5760

// maxOpusChannels is the channel count this bridge supports. openStream rejects
// anything outside 1..2 before a decode starts, so pcmReservation's clamp to this
// value is defence in depth rather than a reachable path.
const maxOpusChannels = 2

// The reservation ceiling (reservation.MaxPCMReservation) and the trim policy
// (reservation.ShouldTrim, reservation.MaxRetainedSlack) live in internal/
// reservation, shared with flacm4a; that package documents the general rule. The
// reservation here is derived from a file's own description, so it needs a bound
// that does not scale with what the file claims.

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
	return DecodeInterleavedLimit(r, int(defaultMaxDecodedBytes.Load()))
}

// defaultMaxDecodedBytes is the ceiling DecodeInterleaved delegates with. It is
// a variable rather than the constant itself only so that a test can lower it:
// asserting that the wrapper is bounded otherwise costs a decode past the real
// default, and a test that cannot afford that ends up asserting nothing, which
// is exactly how a "delegate with no limit" regression would slip through.
//
// It is atomic because the alternative is safe only by an ordering invariant
// nothing enforces: a plain variable is race-free here purely because Go defers
// parallel tests until the serial ones finish, so adding t.Parallel to the test
// that lowers it, or to any sibling that decodes, introduces a data race with no
// warning. One atomic load per decoded file is not a cost worth that. Whatever
// is stored must fit an int, which the constant does on every supported
// architecture.
var defaultMaxDecodedBytes atomic.Int64

func init() { defaultMaxDecodedBytes.Store(m4a.DefaultMaxDecodedBytes) }

// DecodeInterleavedLimit is DecodeInterleaved with an explicit ceiling on the
// decoded size, returning an error wrapping m4a.ErrDecodeLimit as soon as the
// audio decodes past it. A maxBytes of zero or less means no limit, which
// restores the unbounded behaviour and is for input the caller produced or
// otherwise trusts.
//
// The buffer is sized up front from the packet count the container holds, on the
// assumption of the 20 ms packetization this bridge writes, bounded by a fixed
// reservation ceiling and by maxBytes. A file of longer packets is fully
// supported and simply grows past the estimate, up to maxBytes; a file of shorter
// ones decodes to less than was reserved, and the returned slice is then a
// right-sized copy rather than the whole reservation.
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

	// An empty decode hands back nil rather than the non-nil zero-length slice the
	// pre-sized make produced, so a caller's pcm == nil check and a JSON marshal
	// both read the absent case as absent (null, not ""). This package's own writer
	// refuses to close a track with no frames, so it is only reachable from a
	// foreign or crafted file.
	if len(out) == 0 {
		return nil, info, nil
	}
	// Hand back a right-sized copy when the reservation ran well ahead of the audio
	// that actually decoded, which happens when a file's packets are shorter than
	// the 20 ms the reservation assumes. A returned slice pins its entire backing
	// array, so the caller would otherwise keep that reservation for as long as it
	// keeps the PCM.
	if reservation.ShouldTrim(len(out), cap(out)) {
		out = bytes.Clone(out)
	}
	return out, info, nil
}

// DecodeStream opens an Opus .mp4 and decodes it one packet at a time, handing
// each packet's interleaved little-endian 16-bit PCM to fn. It accumulates
// nothing, so it decodes a stream of any length in memory proportional to a single
// packet, which is what makes it the shape to reach for with input the caller did
// not produce. The PCM carries the same priming and padding DecodeInterleaved
// returns, so the same Info.EncoderDelay and Info.Duration trimming described
// there applies.
//
// The slice handed to fn aliases a buffer reused across packets and is valid only
// until fn returns; fn copies whatever it needs to keep. An error from fn stops
// the decode and is returned as-is, so a caller can break out early on its own
// sentinel. A nil fn is rejected with an error rather than panicking partway
// through a file.
func DecodeStream(r io.ReadSeeker, fn func(pcm []byte) error) (m4a.Info, error) {
	if fn == nil {
		return m4a.Info{}, fmt.Errorf("go-m4a/opusm4a: DecodeStream: nil callback")
	}
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
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: track codec is %v, not Opus: %w", info.Codec, m4a.ErrUnsupported)
	}
	if info.Channels < 1 || info.Channels > maxOpusChannels {
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: unsupported channel count %d: %w", info.Channels, m4a.ErrUnsupported)
	}
	dec, err := opus.NewDecoder(opusRate, info.Channels)
	if err != nil {
		// The rate is constant and the channel count was validated just above, so
		// this is practically unreachable from container input; wrap ErrCorrupt
		// anyway so every bridge decode error carries the demuxer's typed contract.
		return nil, nil, info, fmt.Errorf("go-m4a/opusm4a: new decoder: %w: %w", err, m4a.ErrCorrupt)
	}
	return rd, dec, info, nil
}

// forEachPacket decodes every remaining access unit and hands the PCM to fn.
//
// Three buffers are reused for the whole stream: the access unit, the decoder's
// int16 output, and the little-endian bytes fn sees. The access-unit buffer grows
// only when a packet needs more room, which ReadFrameInto reports without
// consuming the packet, so the retry reads the same one; that is an allocation
// per new largest packet, a handful over a whole stream, rather than the one per
// packet ReadFrame costs. The byte buffer replaces the two-bytes-at-a-time append
// this loop used to grow from nil.
func forEachPacket(rd *m4a.Reader, dec *opus.Decoder, channels int, fn func(pcm []byte) error) error {
	var au []byte
	samples := make([]int16, maxSamplesPerChannel*channels)
	pcm := make([]byte, len(samples)*2)
	for {
		n, err := rd.ReadFrameInto(au)
		if errors.Is(err, io.ErrShortBuffer) {
			// Grow geometrically rather than to exactly this frame. A stream whose
			// frames grow monotonically would otherwise reallocate once per frame;
			// doubling caps that at a handful for any stream. An oversized buffer is
			// fine, since ReadFrameInto reads only the frame and reports its size.
			au = make([]byte, max(n, 2*len(au)))
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
			// The container framed this access unit but its Opus payload will not
			// decode: corrupt input. Wrap ErrCorrupt to match the demuxer's contract.
			return fmt.Errorf("go-m4a/opusm4a: decode packet: %w: %w", err, m4a.ErrCorrupt)
		}
		// Defensive: the decoder cannot return more than the buffer it was given, so
		// this never fires today. It costs one comparison per packet and turns a
		// future upstream change from a panic into an error. Compared per channel
		// rather than as got*channels, which would overflow int for a large enough
		// got and let exactly the panic this guards against through.
		if got < 0 || got > maxSamplesPerChannel {
			// Unreachable today (the decoder cannot exceed the buffer it was handed),
			// but wrap ErrCorrupt so every decode-path error honors the same typed
			// contract as the frame/packet decode failures above.
			return fmt.Errorf("go-m4a/opusm4a: decoder returned %d samples per channel, more than the %d it was given room for: %w", got, maxSamplesPerChannel, m4a.ErrCorrupt)
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
// under reservation.MaxPCMReservation before the int conversion, so it fits a 32-bit int too.
func pcmReservation(frameCount, channels, limit int) int {
	if frameCount <= 0 || channels <= 0 {
		return 0
	}
	samples := min(uint64(frameCount), reservation.MaxPCMReservation) * frameSamplesPerChannel
	n := min(samples*uint64(min(channels, maxOpusChannels)), reservation.MaxPCMReservation)
	n = min(n*2, reservation.MaxPCMReservation) // 16-bit samples
	if limit > 0 {
		n = min(n, uint64(limit))
	}
	return int(n)
}
