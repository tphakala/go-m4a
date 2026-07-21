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
)

// encoderBlockSize mirrors the fixed block size go-flac's FrameEncoder emits
// (pcm.encoderBlockSize, which is unexported so it cannot be read from here).
// Only the final block of a stream is shorter. No output depends on the value:
// it sizes the frame slice and nothing else, so drift upstream costs regrows
// rather than correctness. TestFrameReservationCoversEncoder pins it against
// what go-flac actually emits so that drift surfaces as a failure here instead
// of as a silent slowdown.
const encoderBlockSize = 4096

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

// maxPCMReservation is the ceiling on what an accumulating decode reserves up
// front. It bounds the RESERVATION only. What bounds the decode is the caller's
// limit, which pcmReservation also applies (see DecodeInterleavedLimit and
// m4a.DefaultMaxDecodedBytes); this constant is what keeps a file's own
// self-description from driving a large speculative allocation before the first
// frame has decoded.
//
// The bounds pcmReservation derives from the container narrow the honest cases,
// but they cannot make the reservation safe on their own, because FLAC's
// compression ratio has no lower limit: a constant subframe encodes 65535
// samples in a handful of bytes, so a crafted file buys the largest permitted
// block for almost nothing: an access unit carrying one costs on the order of
// fifty bytes plus its sample-table entry, and thirty-three of them reach this
// ceiling, so a file of a couple of kilobytes still gets there.
//
// 64 MiB is about six minutes of 48 kHz stereo 16-bit. The value is a deliberate
// trade and was measured rather than guessed. Lowering it to 8 MiB was tried and
// reverted: it bounds a crafted file to 8 MiB instead of 64 MiB, which is a weak
// gain given that both are transient and freed and that the unbounded decode
// dwarfs either, and it costs every honest clip past 43 seconds its exact
// reservation. Measured on a three-minute stereo clip, that gave up essentially
// all of the benefit, allocating about three and a half times what this ceiling
// does. Honest files are what this constant is tuned for.
const maxPCMReservation = 64 << 20

// maxRetainedSlack is the floor below which an accumulating decode never bothers
// copying the returned slice down to size. It is not the whole rule and not the
// worst case: shouldTrim also requires the slack to be disproportionate, so what
// can actually be handed back is max(maxRetainedSlack, length/2), which for a
// buffer near the ceiling is over 20 MiB.
//
// That is a deliberate trade rather than an oversight. Recovering half a buffer
// costs copying all of it, so trimming a 38 MB result to reclaim 19 MB is not
// obviously worth doing, whereas the case this exists for, a file that declared
// orders of magnitude more audio than it carried, clears any such threshold
// easily. The cost is that a moderately over-declared file, a truncated
// recording being the realistic one, keeps proportional slack. See shouldTrim
// for why the proportional test cannot simply be dropped.
const maxRetainedSlack = 64 << 10

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

	// Encode all frames up front so StreamInfoBytes carries the measured min/max
	// frame sizes and MD5 before the dfLa box is built at Close.
	type frame struct {
		data      []byte
		blockSize int
	}
	// Reserve the frame slice up front rather than growing it from nil and copying
	// every intermediate, the same chain #8 removed from aacm4a. The count is
	// predictable because samplesPerChannel is already known and go-flac emits
	// fixed-size blocks. This is only a capacity hint: a wrong count costs regrows,
	// never correctness. It is also the smaller of the two allocations in this
	// loop; the bytes.Clone per frame below dominates, and is left alone here.
	frames := make([]frame, 0, frameReservation(samplesPerChannel))
	err = fe.EncodeInterleaved(pcm, func(fr []byte, blockSize int) error {
		frames = append(frames, frame{data: bytes.Clone(fr), blockSize: blockSize})
		return nil
	})
	if err != nil {
		return fmt.Errorf("go-m4a/flacm4a: encode: %w", err)
	}

	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		Codec:      m4a.CodecFLAC,
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		STREAMINFO: fe.StreamInfoBytes(),
		// FLAC frames decode to exactly the input samples with no priming, so there
		// is nothing to trim: omit the edit list.
		EncoderDelay: m4a.NoEdit,
	})
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := wr.WriteFrameDuration(f.data, uint32(f.blockSize)); err != nil {
			return err
		}
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

	// Hand back a right-sized copy when the reservation ran well ahead of the audio
	// that actually decoded. A returned slice pins its entire backing array, so a
	// file that overstates its length would otherwise leave the caller holding that
	// reservation for as long as it keeps the PCM.
	if shouldTrim(len(out), cap(out)) {
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
		return nil, nil, info, fmt.Errorf("go-m4a/flacm4a: new frame decoder: %w", err)
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
			return fmt.Errorf("go-m4a/flacm4a: decode frame: %w", err)
		}
		if err := fn(pcm); err != nil {
			return err
		}
	}
}

// shouldTrim reports whether a decoded buffer of the given length and capacity
// is carrying enough dead capacity to be worth copying down to size.
//
// Both tests are load-bearing. The absolute one ignores small overshoot, which is
// not worth a copy. The proportional one keeps the trim off honest files, and it
// is the whole reason this is a function rather than one condition inline: a
// buffer that reached its length through append carries up to a quarter of that
// length as growth headroom, so an absolute threshold alone fires on essentially
// every stream past a megabyte and charges it a full extra copy. Measured on a
// 9-minute clip, that copy cost about 104 MB, and it fell hardest on streams
// declaring an unknown length, where the reservation never engages at all so the
// copy buys nothing whatsoever.
//
// The divisor is half rather than a quarter deliberately. Growth headroom runs to
// about a quarter of the length, so a quarter is exactly on the boundary and was
// measured still firing on an honest 30-second unknown-length stream. What the
// trim is for is the disproportionate case, where a file declared far more audio
// than it carried and the slack dwarfs the audio instead of being a fraction of
// it; that case clears any of these divisors by orders of magnitude.
func shouldTrim(length, capacity int) bool {
	slack := capacity - length
	return slack > maxRetainedSlack && slack > length/2
}

// frameReservation returns the number of FLAC frames that samplesPerChannel
// samples encode to, for use as a slice capacity. The round-up is a remainder
// test rather than the usual (n + blockSize - 1) / blockSize because that form
// is overflow-free whatever it is handed, so the function stays safe if a future
// caller reaches it with a larger value than today's does. Today's cannot
// overflow either form: samplesPerChannel is len(pcm) divided by a stride of at
// least 2, so it never exceeds half the int range.
func frameReservation(samplesPerChannel int) int {
	if samplesPerChannel <= 0 {
		return 0
	}
	n := samplesPerChannel / encoderBlockSize
	if samplesPerChannel%encoderBlockSize != 0 {
		n++
	}
	return n
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
// generous ceiling. maxPCMReservation is what actually bounds the damage, and
// the reasoning for its size lives there. This function narrows the honest
// cases; it does not make a hostile one safe.
//
// Overflow safety is by construction, not by argument: every operand is pinned
// to a bound before it is multiplied and the running total is pinned again after.
// At the current ceiling the largest product is frameCount against
// maxFLACBlockSize, just under 2^42, and the other two reach exactly 2^29 and
// 2^28; all are far inside uint64, and the result is pinned to maxPCMReservation
// before the int conversion, so it fits a 32-bit int too. bytesPerSample needs no
// separate clamp, since (min(bitDepth, 32) + 7) / 8 is at most 4 by its own
// arithmetic. These figures scale with maxPCMReservation, so they are worth
// recomputing rather than trusting if that constant ever moves again.
//
// A declared count of zero means unknown, which STREAMINFO is allowed to say, and
// a file with no frames decodes to nothing. Either way the reservation is zero
// and the buffer simply grows as it did before.
//
// limit is the caller's ceiling on the decoded size (zero or less for none), and
// caps the reservation as well. Without that, a caller who allowed a megabyte
// would still watch a hostile self-description drive a speculative allocation up
// to maxPCMReservation before the first frame decoded, which is most of what the
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

	samples := min(totalSamples, maxPCMReservation)
	samples = min(samples, min(uint64(frameCount), maxPCMReservation)*maxFLACBlockSize)

	bytesPerSample := (min(bitDepth, maxFLACBitDepth) + 7) / 8
	n := min(samples*uint64(min(channels, maxFLACChannels)), maxPCMReservation)
	n = min(n*uint64(bytesPerSample), maxPCMReservation)
	if limit > 0 {
		n = min(n, uint64(limit))
	}
	return int(n)
}
