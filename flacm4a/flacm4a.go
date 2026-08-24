// SPDX-License-Identifier: MIT

// Package flacm4a is an optional convenience bridge that couples go-flac's FLAC
// codec to the go-m4a container. EncodeInterleaved takes interleaved PCM straight
// to a FLAC .mp4 (fLaC/dfLa); DecodeInterleaved opens one and returns the decoded
// PCM, bounded at m4a.DefaultMaxDecodedBytes, and DecodeStream decodes a file of
// any length a frame at a time without accumulating it, which is the shape to
// reach for with input the caller did not produce. It is a thin seam over the two
// libraries: the core m4a package stays stdlib-only, and only this subpackage
// imports go-flac, so a consumer that merely muxes or demuxes never pulls the
// codec.
package flacm4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	flacpcm "github.com/tphakala/go-flac/pcm"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/reservation"
)

// maxFLACBlockSize is the largest number of samples one FLAC frame can hold.
// RFC 9639 section 9.1.1 codes an escaped block size as a 16-bit field holding
// blocksize-1, and section 9.1.6 forbids the value 65535 there (a block of
// 65536) because STREAMINFO's own 16-bit field could not represent it. It bounds
// a whole track only because the MP4 mapping lines frames up with access units
// one to one: "a FLAC sample is exactly one FLAC frame" (Xiph, Encapsulation of
// FLAC in ISO Base Media File Format, sections 3.2 and 3.3.3).
//
// go-flac does not enforce the prohibition (internal/frame/header.go reads the
// 16 bits and adds one, with no check against STREAMINFO), so a non-conforming
// file can decode 65536-sample blocks. That direction is harmless here: it makes
// the bound one sample per frame too low, which costs a regrow, never a
// correctness or a safety problem.
const maxFLACBlockSize = 65535

// The reservation ceiling (reservation.MaxPCMReservation) and the trim policy
// (reservation.ShouldTrim, reservation.MaxRetainedSlack) live in internal/
// reservation, shared with opusm4a; that package documents the general rule. The
// container bounds pcmReservation derives below cannot make the reservation safe
// on their own, which is why the ceiling exists: FLAC's compression ratio has no
// lower limit, so a constant subframe encodes a 65535-sample block in a handful
// of bytes, and a crafted file of a couple of kilobytes still reaches any
// per-claim estimate.

// Config configures FLAC encoding. SampleRate is the audio rate in Hz; Channels is
// 1 or 2; BitDepth is 16 or 24; CompressionLevel is 0 (fastest) to 8 (smallest).
type Config struct {
	SampleRate       int
	Channels         int
	BitDepth         int
	CompressionLevel int
}

// EncodeInterleaved encodes interleaved little-endian PCM as a FLAC .mp4 to w. FLAC
// is lossless and has no encoder priming, so no edit list is written and a decode
// reproduces the input exactly.
//
// Frames stream straight into w as they are encoded, so on success w holds the
// whole file, but on an encode or write error w may already hold a partial,
// unfinalized file (the ftyp box, the mdat header, and any frames written before
// the failure). A caller that reuses w should discard or truncate it on error.
func EncodeInterleaved(w io.WriteSeeker, cfg Config, pcm []byte) error {
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return fmt.Errorf("go-m4a/flacm4a: channels %d out of range, want 1 or 2", cfg.Channels)
	}
	// Require a byte-aligned bit depth so the interleaved-PCM stride is exact; a
	// non-byte-aligned depth (for example 20) would divide to the wrong stride and
	// mis-parse the buffer before go-flac ever saw the config.
	if cfg.BitDepth != 16 && cfg.BitDepth != 24 {
		return fmt.Errorf("go-m4a/flacm4a: bit depth %d unsupported, want 16 or 24", cfg.BitDepth)
	}
	stride := cfg.Channels * (cfg.BitDepth / 8)
	if len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/flacm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}
	samplesPerChannel := len(pcm) / stride

	fe, err := flacpcm.NewFrameEncoder(flacpcm.Config{
		SampleRate:       cfg.SampleRate,
		Channels:         cfg.Channels,
		BitDepth:         cfg.BitDepth,
		CompressionLevel: cfg.CompressionLevel,
		TotalSamples:     uint64(samplesPerChannel),
	})
	if err != nil {
		return fmt.Errorf("go-m4a/flacm4a: new frame encoder: %w", err)
	}

	// Open the writer up front with an empty STREAMINFO. go-flac's StreamInfoBytes
	// is only final after the whole encode (it carries the measured min/max frame
	// sizes and MD5), but the dfLa box is not built until Close, so the writer does
	// not need STREAMINFO until then. Supplying it late via SetSTREAMINFO lets each
	// frame stream straight into the mdat payload as it is encoded, replacing the
	// per-clip arena and frame-record buffering the up-front encode used to need.
	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		Codec:      m4a.CodecFLAC,
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		// FLAC frames decode to exactly the input samples with no priming, so there
		// is nothing to trim: omit the edit list.
		EncoderDelay: m4a.NoEdit,
	})
	if err != nil {
		return err
	}

	// Write each frame directly from the encode callback. go-flac reuses the buffer
	// it hands the callback, but WriteFrameDuration copies the access unit into the
	// mdat stream before returning, so nothing has to outlive the call. A write
	// failure is surfaced verbatim (it already carries the "go-m4a:" prefix) rather
	// than being re-wrapped as an encode error.
	var writeErr error
	encErr := fe.EncodeInterleaved(pcm, func(fr []byte, blockSize int) error {
		if err := wr.WriteFrameDuration(fr, uint32(blockSize)); err != nil {
			writeErr = err
			return err
		}
		return nil
	})
	if writeErr != nil {
		return writeErr
	}
	if encErr != nil {
		return fmt.Errorf("go-m4a/flacm4a: encode: %w", encErr)
	}

	// StreamInfoBytes is final now that every frame is encoded; supply it before
	// Close builds the dfLa box.
	if err := wr.SetSTREAMINFO(fe.StreamInfoBytes()); err != nil {
		return err
	}
	return wr.Close()
}

// DecodeInterleaved opens a FLAC .mp4 and decodes it to interleaved little-endian
// PCM, returning the PCM together with the container Info. FLAC is lossless, so the
// PCM is bit-identical to what EncodeInterleaved was given.
//
// The decode is bounded at m4a.DefaultMaxDecodedBytes: a stream that decodes to
// more stops with an error wrapping m4a.ErrDecodeLimit instead of growing the
// buffer to fit. That bound is what makes this safe to point at a file the caller
// did not produce, because FLAC's compression ratio has no lower limit, so the
// decoded size is not proportional to the file. Use DecodeInterleavedLimit to
// choose the ceiling, or DecodeStream to decode a stream of any length without
// accumulating it.
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
// The buffer is sized up front from what the file declares, bounded by what the
// container corroborates, by a fixed reservation ceiling and by maxBytes, so a
// file claiming an implausible length cannot make the decoder reserve for the
// claim. Streams longer than the reservation ceiling are fully supported and
// simply grow past it, up to maxBytes.
func DecodeInterleavedLimit(r io.ReadSeeker, maxBytes int) ([]byte, m4a.Info, error) {
	rd, fd, info, err := openStream(r)
	if err != nil {
		return nil, info, err
	}

	// Reserve the whole decoded stream up front rather than letting append grow it
	// a frame at a time: once a clip runs to hundreds of frames the growth chain
	// copies roughly four times the final size (measured; the ratio approaches 5
	// asymptotically but is well below it at real clip lengths, and is zero for a
	// clip of a single block). pcmReservation trusts neither the declared length
	// nor the container alone, and bounds one against the other.
	// The decode itself follows STREAMINFO, since that is what the decoder was
	// built from; the container's channel count only narrows the buffer estimate,
	// and a short estimate costs regrows rather than correctness.
	si := fd.StreamInfo()
	out := make([]byte, 0, pcmReservation(si.TotalSamples, info.FrameCount, si.Channels, info.Channels, si.BitDepth, maxBytes))
	err = forEachFrame(rd, fd, func(pcm []byte) error {
		// Written as a subtraction so the test cannot overflow int on a 32-bit
		// build. len(out) never exceeds maxBytes, so the difference is non-negative,
		// and a frame that lands exactly on the limit is a fit rather than an excess.
		if maxBytes > 0 && len(pcm) > maxBytes-len(out) {
			return fmt.Errorf("go-m4a/flacm4a: decoded output exceeds the %d-byte limit: %w", maxBytes, m4a.ErrDecodeLimit)
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
	// that actually decoded. A returned slice pins its entire backing array, so a
	// file that overstates its length would otherwise leave the caller holding that
	// reservation for as long as it keeps the PCM.
	if reservation.ShouldTrim(len(out), cap(out)) {
		out = bytes.Clone(out)
	}
	return out, info, nil
}

// DecodeStream opens a FLAC .mp4 and decodes it one access unit at a time, handing
// each frame's interleaved little-endian PCM to fn. It accumulates nothing, so it
// decodes a stream of any length in memory proportional to a single frame, which
// is what makes it the shape to reach for with input the caller did not produce.
// There is no encode counterpart yet: EncodeInterleaved takes the whole PCM
// buffer at once.
//
// The slice handed to fn aliases a buffer the decoder reuses across frames and is
// valid only until fn returns; fn copies whatever it needs to keep. An error from
// fn stops the decode and is returned as-is, so a caller can break out early on
// its own sentinel. A nil fn is rejected with an error rather than panicking
// partway through a file.
func DecodeStream(r io.ReadSeeker, fn func(pcm []byte) error) (m4a.Info, error) {
	if fn == nil {
		return m4a.Info{}, fmt.Errorf("go-m4a/flacm4a: DecodeStream: nil callback")
	}
	rd, fd, info, err := openStream(r)
	if err != nil {
		return info, err
	}
	return info, forEachFrame(rd, fd, fn)
}

// openStream opens r as a FLAC .mp4 and builds the frame decoder its STREAMINFO
// describes. It is the shared prologue of the decode entry points.
func openStream(r io.ReadSeeker) (*m4a.Reader, *flacpcm.FrameDecoder, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, nil, m4a.Info{}, err
	}
	info := rd.Info()
	if info.Codec != m4a.CodecFLAC {
		return nil, nil, info, fmt.Errorf("go-m4a/flacm4a: track codec is %v, not FLAC: %w", info.Codec, m4a.ErrUnsupported)
	}
	fd, err := flacpcm.NewFrameDecoder(info.CodecConfig)
	if err != nil {
		// NewReader accepted the container, but its STREAMINFO does not build a
		// decoder: corrupt from the bridge's point of view. Wrap ErrCorrupt so the
		// decode path carries the same typed contract as the demuxer's rejections.
		return nil, nil, info, fmt.Errorf("go-m4a/flacm4a: new frame decoder: %w: %w", err, m4a.ErrCorrupt)
	}
	return rd, fd, info, nil
}

// forEachFrame decodes every remaining access unit and hands the PCM to fn.
//
// The access-unit buffer is reused across frames and grown only when a frame needs
// more room: ReadFrameInto reports the size it wants without consuming the frame,
// so the retry reads the same access unit. That keeps the loop to an allocation
// per new largest frame, a handful over a whole stream, rather than the one per
// access unit ReadFrame costs, and it bounds the buffer by the largest frame the
// container actually holds rather than by anything the file declares.
func forEachFrame(rd *m4a.Reader, fd *flacpcm.FrameDecoder, fn func(pcm []byte) error) error {
	var au []byte
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
		pcm, _, err := fd.DecodeInterleaved(au[:n])
		if err != nil {
			// The container framed this access unit but its FLAC payload will not
			// decode: corrupt input. Wrap ErrCorrupt to match the demuxer's contract.
			return fmt.Errorf("go-m4a/flacm4a: decode frame: %w: %w", err, m4a.ErrCorrupt)
		}
		if err := fn(pcm); err != nil {
			return err
		}
	}
}

// pcmReservation returns the byte capacity to reserve for a decoded stream,
// given what the file's STREAMINFO declares (totalSamples, channels, bitDepth)
// and how many access units the container actually holds (frameCount).
//
// STREAMINFO is a self-description, so the declared length is believed only up
// to what the container can corroborate: frameCount comes from the sample table
// the reader has already validated against the real file length, and one access
// unit is one FLAC frame of at most maxFLACBlockSize samples. A truncated file,
// which declares its original length while carrying part of the audio, is
// brought back to roughly what it holds by that bound alone.
//
// Be clear about what this does not do. The bound is weak against a file built
// to defeat it, because the per-access-unit budget (maxFLACBlockSize samples at
// the widest permitted stride) is around two megabytes while an access unit
// costs the attacker about a byte, so a few dozen of them still reach any
// generous ceiling. reservation.MaxPCMReservation is what actually bounds the damage, and
// the reasoning for its size lives there. This function narrows the honest
// cases; it does not make a hostile one safe.
//
// Overflow safety is by construction, not by argument: every operand is pinned
// to a bound before it is multiplied and the running total is pinned again after.
// At the current ceiling the largest product is frameCount against
// maxFLACBlockSize, just under 2^42, and the other two reach exactly 2^29 and
// 2^28; all are far inside uint64, and the result is pinned to reservation.MaxPCMReservation
// before the int conversion, so it fits a 32-bit int too. bytesPerSample needs no
// separate clamp, since (min(bitDepth, 32) + 7) / 8 is at most 4 by its own
// arithmetic. These figures scale with reservation.MaxPCMReservation, so they are worth
// recomputing rather than trusting if that constant ever moves again.
//
// A declared count of zero means unknown, which STREAMINFO is allowed to say, and
// a file with no frames decodes to nothing. Either way the reservation is zero
// and the buffer simply grows as it did before.
//
// limit is the caller's ceiling on the decoded size (zero or less for none), and
// caps the reservation as well. Without that, a caller who allowed a megabyte
// would still watch a hostile self-description drive a speculative allocation up
// to reservation.MaxPCMReservation before the first frame decoded, which is most of what the
// limit exists to prevent. It is a ceiling and never a floor: a limit above what
// the stream declares leaves the reservation where the declaration put it.
func pcmReservation(totalSamples uint64, frameCount, siChannels, seChannels, bitDepth, limit int) int {
	// FLAC caps a stream at 8 channels and 32 bits per sample.
	const (
		maxFLACChannels = 8
		maxFLACBitDepth = 32
	)
	if totalSamples == 0 || frameCount <= 0 || siChannels <= 0 || bitDepth <= 0 {
		return 0
	}
	// The channel count is stated twice, by STREAMINFO and by the container's
	// sample entry, and the mapping requires them to agree, so take the smaller: a
	// file that inflates one to widen the reservation has to inflate both. Only a
	// positive sample-entry count counts as a statement, though. Nothing validates
	// that field, and a muxer that leaves it zero is saying nothing rather than
	// saying none; letting a zero win the comparison would disable the reservation
	// outright and hand the decode back the growth chain this exists to remove.
	channels := siChannels
	if seChannels > 0 && seChannels < channels {
		channels = seChannels
	}

	samples := min(totalSamples, reservation.MaxPCMReservation)
	samples = min(samples, min(uint64(frameCount), reservation.MaxPCMReservation)*maxFLACBlockSize)

	bytesPerSample := (min(bitDepth, maxFLACBitDepth) + 7) / 8
	n := min(samples*uint64(min(channels, maxFLACChannels)), reservation.MaxPCMReservation)
	n = min(n*uint64(bytesPerSample), reservation.MaxPCMReservation)
	if limit > 0 {
		n = min(n, uint64(limit))
	}
	return int(n)
}
