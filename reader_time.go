// SPDX-License-Identifier: MIT

package m4a

import (
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// SeekTime moves the running cursor to the access unit that holds presentation
// time d, so the next ReadFrame, ReadFrameInto or RawStream read returns it, and
// reports how many decoded samples per channel to drop from the start of that
// unit's output to land exactly on d. It does no I/O: the unit comes from the
// timing table built at open (stts for a plain file, the trun durations for a
// fragmented stream) and the cursor move is SeekSample's.
//
// Presentation time 0 is the first presented sample: media tick
// Info().EncoderDelay, the priming the edit list skips, after any leading empty
// edits (whose length is Info().StartOffset and is not part of the time axis). A
// seek to 0 selects the first sample a gapless player presents and SeekTime(d)
// selects the same sample as decoding from the start and dropping EncoderDelay
// plus d's worth of samples. d is rounded to the nearest media tick (halves up),
// so a time derived from a tick through a float or a truncated nanosecond count
// still lands on that tick's unit.
//
// skip counts samples at Info().SampleRate. It equals the offset in media ticks
// whenever the media timescale equals the sample rate, which holds for every file
// this package writes; otherwise it is rescaled and rounded down. A unit with a
// zero duration is never returned.
//
// The valid range is [0, Info().Duration): the window the edit list presents,
// the same one Info().Duration reports, so trailing padding is excluded. A d
// outside it, including the last half tick before the end, returns an error
// wrapping ErrOutOfRange with the cursor unchanged. A track with no sample
// timing, or whose edit list does not reduce to one presented window (more than
// one non-empty edit, a trailing empty edit, a non-unit media rate, no non-empty
// edit), returns an error wrapping ErrUnsupported for any d >= 0, also with the
// cursor unchanged; a negative d is always ErrOutOfRange. On any error the
// results are 0, 0.
//
// The result depends only on d and the file, never on the cursor, so SeekTime may
// be repeated and may be called after the end of the stream. As for SeekSample,
// discard an io.Reader from an earlier RawStream call and start a new decoder.
// The bridges' decoders are not edit-list aware and a lossy decoder started cold
// at the returned unit has no overlap state from the unit before it, so the first
// units it emits are degraded: a caller that wants clean output seeks a few units
// earlier with SeekSample and discards that output as well as skip samples.
func (rd *Reader) SeekTime(d time.Duration) (sample int, skip int64, err error) {
	if d < 0 {
		return 0, 0, fmt.Errorf("go-m4a: seek to negative time %v: %w", d, ErrOutOfRange)
	}
	if rd.window.seekErr != nil {
		return 0, 0, rd.window.seekErr
	}
	ticks, ok := rescaleTicks(uint64(d), uint32(time.Second), rd.timescale)
	if !ok {
		return 0, 0, fmt.Errorf("go-m4a: seek time %v overflows the media timescale: %w", d, ErrOutOfRange)
	}
	target, carry := bits.Add64(rd.window.start, ticks, 0)
	if carry != 0 || target >= rd.window.end {
		return 0, 0, fmt.Errorf("go-m4a: seek time %v is at or past the presented end: %w", d, ErrOutOfRange)
	}
	idx, off, ok := locateTick(rd.timeRuns, target)
	// Unreachable while target < window.end <= the summed durations and the runs
	// cover every sample; kept so a table inconsistency is an error, not a wrong unit.
	if !ok || idx >= int64(rd.sampleCount) {
		return 0, 0, fmt.Errorf("go-m4a: seek time %v is beyond the timing table: %w", d, ErrOutOfRange)
	}
	if err := rd.SeekSample(int(idx)); err != nil {
		return 0, 0, err
	}
	// off is below one unit's uint32 delta and the rate below 2^32, so the product
	// fits uint64.
	if rate := uint64(rd.info.SampleRate); rate > 0 && rate != uint64(rd.timescale) {
		off = off * rate / uint64(rd.timescale)
	}
	return int(idx), int64(off), nil
}

// appendTimeRun appends count samples of the given duration to runs, dropping an
// empty run, merging into the previous run when the duration is equal and
// starting a new one instead of letting its Count pass MaxUint32.
func appendTimeRun(runs []box.SttsRun, count, delta uint32) []box.SttsRun {
	if count == 0 {
		return runs
	}
	if n := len(runs); n > 0 && runs[n-1].Delta == delta {
		room := math.MaxUint32 - runs[n-1].Count
		if count <= room {
			runs[n-1].Count += count
			return runs
		}
		runs[n-1].Count = math.MaxUint32
		count -= room
	}
	return append(runs, box.SttsRun{Count: count, Delta: delta})
}

// sumTicks returns the total duration of runs in media ticks, saturating at
// MaxUint64. Each Count*Delta is a product of two uint32 values, so it fits.
func sumTicks(runs []box.SttsRun) uint64 {
	var sum uint64
	for _, r := range runs {
		var carry uint64
		sum, carry = bits.Add64(sum, uint64(r.Count)*uint64(r.Delta), 0)
		if carry != 0 {
			return math.MaxUint64
		}
	}
	return sum
}

// rescaleTicks converts v from a timescale of from ticks per second to one of to,
// rounding to nearest with halves up. ok is false when from is zero or the result
// does not fit uint64.
func rescaleTicks(v uint64, from, to uint32) (uint64, bool) {
	if from == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(v, uint64(to))
	var carry uint64
	lo, carry = bits.Add64(lo, uint64(from/2), 0)
	hi += carry // hi < 2^32, so this cannot wrap
	if hi >= uint64(from) {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, uint64(from))
	return q, true
}

// locateTick finds the sample whose duration span holds target, returning its
// index and the tick offset into it. A run with a zero Delta contributes samples
// but no time, so its samples are never returned. ok is false when target is at
// or past the total. Count and Delta are widened before every product, since a
// uint32 product would truncate silently.
func locateTick(runs []box.SttsRun, target uint64) (sample int64, off uint64, ok bool) {
	var acc, idx uint64
	for _, r := range runs {
		if r.Delta == 0 {
			idx += uint64(r.Count)
			continue
		}
		rem := target - acc // acc <= target: only runs wholly below target are added
		k := rem / uint64(r.Delta)
		if k < uint64(r.Count) {
			return int64(idx + k), rem - k*uint64(r.Delta), true
		}
		acc += uint64(r.Count) * uint64(r.Delta)
		idx += uint64(r.Count)
	}
	return 0, 0, false
}
