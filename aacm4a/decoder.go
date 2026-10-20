// SPDX-License-Identifier: MIT

package aacm4a

import (
	"errors"
	"fmt"
	"io"
	"time"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"

	m4a "github.com/tphakala/go-m4a"
)

const (
	// seekPreRoll is how many access units a seek decodes and discards ahead of
	// the target. One unit restores everything an AAC-LC decoder carries across
	// frames apart from PNS: the MDCT overlap-add buffer and the window shape of
	// the previous frame (TNS is per frame, and LTP does not exist in AAC-LC).
	// Measured against a linear decode on go-aac encodes with PNS disabled, short
	// windows and TNS included, a pre-roll of 1 and of 2 are both bit-exact at
	// every target; the second unit is margin against any one-frame state not
	// identified, at the cost of one extra unit decoded per seek. More buys
	// nothing measurable, and no amount of pre-roll restores PNS state.
	seekPreRoll = 2

	// maxAccessUnit is the largest access unit the decoder accepts, the same
	// ceiling the m4a.Reader's RawStream enforces, so NewDecoder and the
	// seekable decoder refuse the same files. It also keeps a sparse file with a
	// huge sample size entry from driving a large allocation.
	maxAccessUnit = 0xffff
)

// Decoder streams the PCM of an AAC-LC .m4a as interleaved little-endian S16,
// exactly like the go-aac decoder NewDecoder returns, and adds random access:
// SeekSample and SeekTime reposition the output, and the decoder handles the
// codec pre-roll itself. Build one with NewSeekableDecoder. NewDecoder returns
// an *aacpcm.Decoder, not this type.
//
// Without a seek the output is byte for byte what NewDecoder produces, with one
// difference: an access unit that carries no audio element makes Read fail with
// an error matching aacpcm.ErrCorruptStream and m4a.ErrCorrupt, where
// NewDecoder's decoder silently emits nothing for it.
//
// Their errors also differ when the stream ends before a frame the sample table
// places inside it: Read returns a wrapped io.ErrUnexpectedEOF, where
// NewDecoder's decoder reports aacpcm.ErrCorruptStream wrapping it.
//
// The output is not edit-list aware: it holds every decoded sample, priming and
// final-frame padding included, as NewDecoder's does. After SeekSample(i) the
// next byte is the one at sample i*1024 of that output. After SeekTime(t) it is
// the sample at presentation time t, that is (Info().EncoderDelay + round(t *
// SampleRate)) when the file's sample durations are all 1024 at a media
// timescale equal to the sample rate, which holds for every file this package
// writes. The output then runs to the end of the decoded track, trailing padding
// included: the caller keeps presented - round(t*SampleRate) samples per
// channel, where presented is Info().Duration at SampleRate rounded up (Duration
// is a floor).
//
// Noise substitution caveat: AAC perceptual noise substitution (PNS) draws its
// noise from a generator that runs across the whole stream, and a seek cannot
// reconstruct that state. On a stream that uses PNS (go-aac's encoder emits it by
// default on noisy material, ffmpeg's native encoder does too), the output after
// a seek therefore differs sample for sample from a linear decode in the
// noise-substituted bands for the rest of the stream. Sample position and length
// are still exact, the output is identical for the same target on every run, and
// measured on noisy go-aac encodes the per-frame energy stays within 0.1 dB of
// the linear decode with a correlation of at least 0.96. A stream with no PNS
// band is bit-exact after a seek, and so is a seek to one of the first
// access units.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	rd         *m4a.Reader
	fd         *aacpcm.FrameDecoder
	frameCount int    // access units in the track, for SeekSample's range check
	stride     int    // bytes per interleaved sample frame: channels*2, from the ASC
	au         []byte // reused access-unit buffer, len <= maxAccessUnit
	pcm        []byte // reused decoded frame
	pending    []byte // unread window into pcm
	discard    int64  // decoded samples per channel still to drop
	next       int    // index of the next access unit to decode, for error messages
	err        error  // latched terminal error, cleared by a successful seek
}

// NewSeekableDecoder opens an MP4/M4A file and returns a Decoder together with
// the container Info, like NewDecoder. opts configure the internal m4a.Reader.
// The track must be AAC: any other codec returns an error matching
// m4a.ErrUnsupported. HE-AAC and HE-AACv2 are rejected as in NewDecoder, and the
// error matches go-aac's aacpcm.ErrUnsupportedSBR or aacpcm.ErrUnsupportedPS as
// well as m4a.ErrUnsupported. On a decoder error the Info is still returned.
func NewSeekableDecoder(r io.ReadSeeker, opts ...m4a.ReaderOption) (*Decoder, m4a.Info, error) {
	rd, err := m4a.NewReader(r, opts...)
	if err != nil {
		return nil, m4a.Info{}, err
	}
	info := rd.Info()
	if info.Codec != m4a.CodecAACLC {
		return nil, info, fmt.Errorf("go-m4a/aacm4a: track codec is %v, not AAC: %w", info.Codec, m4a.ErrUnsupported)
	}
	fd, err := aacpcm.NewRawDecoder(info.ASC)
	if err != nil {
		return nil, info, fmt.Errorf("go-m4a/aacm4a: decoder config: %w", withM4ASentinel(err))
	}
	return &Decoder{
		rd:         rd,
		fd:         fd,
		frameCount: info.FrameCount,
		stride:     fd.Channels() * 2,
	}, info, nil
}

// Info returns the container Info of the opened file.
func (d *Decoder) Info() m4a.Info { return d.rd.Info() }

// Read implements io.Reader. It returns the PCM bytes in order, split anywhere,
// and io.EOF at the end of the track. A decode error is latched: Read returns it
// again until a successful SeekSample or SeekTime.
func (d *Decoder) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if d.err != nil {
		return 0, d.err
	}
	if err := d.fill(); err != nil {
		return 0, err
	}
	n := copy(p, d.pending)
	d.pending = d.pending[n:]
	return n, nil
}

// WriteTo implements io.WriterTo, so io.Copy streams without an intermediate
// buffer.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	if d.err != nil {
		return 0, d.err
	}
	var total int64
	for {
		if err := d.fill(); err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
		n, err := w.Write(d.pending)
		total += int64(n)
		d.pending = d.pending[n:]
		if err != nil {
			return total, err
		}
		if len(d.pending) != 0 {
			return total, io.ErrShortWrite
		}
	}
}

// fill decodes access units until pending holds output, or returns the latched
// error or io.EOF.
func (d *Decoder) fill() error {
	for len(d.pending) == 0 {
		if d.err != nil {
			return d.err
		}
		if err := d.decodeNext(); err != nil {
			return err
		}
	}
	return nil
}

// readAU reads the next access unit into d.au and returns its length. It
// returns io.EOF at the end of the track without latching.
func (d *Decoder) readAU() (int, error) {
	n, err := d.rd.ReadFrameInto(d.au[:min(cap(d.au), maxAccessUnit)])
	if errors.Is(err, io.ErrShortBuffer) {
		if n > maxAccessUnit {
			return 0, d.fail(fmt.Errorf("go-m4a/aacm4a: access unit %d of %d bytes exceeds %d: %w",
				d.next, n, maxAccessUnit, m4a.ErrUnsupported))
		}
		// Grow geometrically so a VBR stream does not reallocate at every new
		// high-water mark.
		d.au = make([]byte, min(max(n, 2*cap(d.au)), maxAccessUnit))
		n, err = d.rd.ReadFrameInto(d.au)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, d.fail(err)
	}
	return n, nil
}

// decodeNext decodes one access unit. It leaves pending empty when the unit is
// consumed by the discard count, and the caller loops.
func (d *Decoder) decodeNext() error {
	n, err := d.readAU()
	if err != nil {
		return err
	}
	out, samples, err := d.fd.DecodeFrame(d.pcm[:0], d.au[:n])
	if err != nil {
		return d.fail(wrapDecodeErr(d.next, err))
	}
	d.pcm = out
	d.next++
	if d.discard >= int64(samples) {
		d.discard -= int64(samples)
		return nil
	}
	d.pending = out[int(d.discard)*d.stride:]
	d.discard = 0
	return nil
}

// SeekSample positions the output at the start of access unit sample's decoded
// PCM, byte sample*1024*stride of NewDecoder's output, where sample is in
// [0, Info().FrameCount). Any other value returns an error wrapping
// m4a.ErrOutOfRange and changes nothing. The seek is lazy: the pre-roll units
// are decoded by the next Read, and a decode error in one of them surfaces
// there. A successful seek clears a latched decode error.
func (d *Decoder) SeekSample(sample int) error {
	if sample < 0 || sample >= d.frameCount {
		return fmt.Errorf("go-m4a/aacm4a: seek to sample %d, track has %d: %w", sample, d.frameCount, m4a.ErrOutOfRange)
	}
	return d.restart(sample, 0, seekPreRoll)
}

// SeekTime positions the output at presentation time t, as m4a.Reader.SeekTime
// defines it: the range is [0, Info().Duration), and an edit list that does not
// reduce to one presented window returns an error wrapping m4a.ErrUnsupported.
// Those errors change nothing. When the seek succeeds, the output starts at the
// sample the type documentation describes. A file whose unit durations are not 1024 at the
// sample rate is mapped through the container's timing, which does not match the
// decoded timeline. A time that lands 1024 or more samples into an access unit,
// which a unit whose container duration exceeds the 1024 samples it decodes to
// allows, returns an error wrapping m4a.ErrUnsupported, drops pending output and
// blocks reads until the next successful seek. The seek is lazy, as SeekSample.
func (d *Decoder) SeekTime(t time.Duration) error {
	idx, skip, err := d.rd.SeekTime(t)
	if err != nil {
		return err
	}
	if skip < 0 || skip >= aac.FrameSize {
		return d.fail(fmt.Errorf("go-m4a/aacm4a: seek time %v lands %d samples into access unit %d, which decodes to %d: %w",
			t, skip, idx, aac.FrameSize, m4a.ErrUnsupported))
	}
	return d.restart(idx, skip, seekPreRoll)
}

// restart moves the reader preRoll units before target, resets the codec and
// records the samples to drop. It overwrites every piece of cursor state, so the
// output after it depends only on the file and the target.
func (d *Decoder) restart(target int, skip int64, preRoll int) error {
	// Clear first: if a later step latches an error, no stale output from before
	// the seek may survive it.
	d.pending = nil
	d.discard = 0
	d.err = nil
	start := max(0, target-preRoll)
	if err := d.rd.SeekSample(start); err != nil {
		return d.fail(err)
	}
	if err := d.fd.Reset(); err != nil {
		return d.fail(wrapDecodeErr(start, err))
	}
	d.next = start
	d.discard = int64(target-start)*aac.FrameSize + skip
	return nil
}

// fail latches err, drops any pending output and returns err.
func (d *Decoder) fail(err error) error {
	d.pending = nil
	d.discard = 0
	d.err = err
	return err
}

// wrapDecodeErr labels a go-aac decode error with the access unit index and the
// matching m4a sentinel, keeping go-aac's sentinel matchable too.
func wrapDecodeErr(idx int, err error) error {
	return fmt.Errorf("go-m4a/aacm4a: decode access unit %d: %w", idx, withM4ASentinel(err))
}

// withM4ASentinel adds m4a.ErrUnsupported or m4a.ErrCorrupt to a go-aac error,
// whichever its sentinel maps to, and returns err as is for anything else.
func withM4ASentinel(err error) error {
	switch {
	case errors.Is(err, aacpcm.ErrUnsupported):
		return fmt.Errorf("%w: %w", err, m4a.ErrUnsupported)
	case errors.Is(err, aacpcm.ErrCorruptStream):
		return fmt.Errorf("%w: %w", err, m4a.ErrCorrupt)
	}
	return err
}
