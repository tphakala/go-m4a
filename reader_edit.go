// SPDX-License-Identifier: MIT

package m4a

import (
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// unitMediaRate is the 16.16 fixed-point media_rate of a normal-speed edit.
const unitMediaRate = 0x00010000

// presentation is the one window of the track's media that the edit list
// presents, resolved once at open. Every value derived from the edit list comes
// from it: Info.EncoderDelay is start, Info.Duration is presented converted to
// time, Info.StartOffset is lead converted to time, and SeekTime accepts media
// tick start+t exactly when start+t < end. For a window SeekTime supports,
// presented == end-start. All values are media ticks except lead, which is in
// the movie timescale.
type presentation struct {
	start     uint64
	end       uint64
	presented uint64
	lead      uint64

	// seekErr is the ErrUnsupported error SeekTime returns for a non-negative
	// time, or nil when the track can be time-sought.
	seekErr error
}

// resolvePresentation reduces the track's edit list to one presentation window.
// runs are the sample durations (stts or trun) and fragmented selects the
// reading of a zero segment_duration: open-ended on a fragmented stream (this
// package's own init segment writes it), literal on a plain file, where it
// presents nothing (ffmpeg does the same).
//
// The media end is the summed sample durations, or the mdhd duration when they
// sum to nothing. The movie header duration is not used: it is in the movie
// timescale and rounded, so it can disagree with the media by a tick.
//
// Without an edit list the window starts at the Opus dOps PreSkip (0 for other
// codecs). With one, leading empty edits (media_time -1) are summed into lead and
// skipped, and the window is the first edit with a media_time >= 0. SeekTime
// supports only that shape: leading empty edits followed by exactly one edit at
// rate 1.0. For any other shape Duration is the sum of the presented lengths of
// every edit with a media_time >= 0, and seekErr says why SeekTime cannot map
// time to media.
func resolvePresentation(tr *track, movieTS uint32, runs []box.SttsRun, fragmented bool) presentation {
	var w presentation
	mediaEnd := sumTicks(runs)
	noTiming := tr.mdhdTS == 0 || mediaEnd == 0
	if mediaEnd == 0 {
		mediaEnd = tr.mdhdDur
	}

	w.start = fallbackStart(tr)
	edits := tr.edits
	if len(edits) == 0 {
		w.end = max(mediaEnd, w.start)
		w.presented = w.end - w.start
		w.setNoTiming(noTiming)
		return w
	}

	// Leading empty edits.
	i := 0
	for i < len(edits) && edits[i].MediaTime == -1 {
		var carry uint64
		w.lead, carry = bits.Add64(w.lead, edits[i].SegmentDuration, 0)
		if carry != 0 {
			w.lead = math.MaxUint64
		}
		i++
	}
	rest := edits[i:]

	haveStart := false
	for _, e := range rest {
		if e.MediaTime < 0 {
			continue
		}
		if !haveStart {
			w.start, haveStart = uint64(e.MediaTime), true
		}
		length := editLength(e, mediaEnd, movieTS, tr.mdhdTS, fragmented)
		var carry uint64
		w.presented, carry = bits.Add64(w.presented, length, 0)
		if carry != 0 {
			w.presented = math.MaxUint64
		}
	}
	var carry uint64
	w.end, carry = bits.Add64(w.start, w.presented, 0)
	if carry != 0 {
		w.end = math.MaxUint64
	}

	if reason := unsupportedShape(rest); reason != "" {
		w.seekErr = fmt.Errorf("go-m4a: time seek on an edit list with %s is unsupported: %w", reason, ErrUnsupported)
	}
	w.setNoTiming(noTiming)
	return w
}

// setNoTiming records the no-timing error, which takes precedence over a shape
// error so the message names the first thing a caller would have to fix.
func (w *presentation) setNoTiming(noTiming bool) {
	if noTiming {
		w.seekErr = fmt.Errorf("go-m4a: track has no sample timing: %w", ErrUnsupported)
	}
}

// fallbackStart is the window start without a usable edit: the Opus dOps
// PreSkip, the canonical Opus priming signal per the Encapsulation of Opus in
// ISOBMFF (RFC 7845), else 0.
func fallbackStart(tr *track) uint64 {
	if tr.codec == fourccOpus {
		return uint64(tr.opusPreSkip)
	}
	return 0
}

// unsupportedShape names why the edits after the leading empty ones are not a
// single rate-1.0 edit with a media_time >= 0, or returns "" when they are.
func unsupportedShape(rest []box.ElstEntry) string {
	switch {
	case len(rest) == 0:
		return "only empty edits"
	case rest[0].MediaTime < 0:
		return "a negative media_time"
	case len(rest) > 1:
		return "more than one edit after the leading empty edits"
	case rest[0].MediaRate != unitMediaRate:
		return "a media rate other than 1.0"
	}
	return ""
}

// editLength returns the presented length of one edit in media ticks:
// segment_duration rescaled from the movie to the media timescale (rounding to
// nearest), clipped to the media end and never negative. A zero segment_duration
// runs to the media end on a fragmented stream and presents nothing on a plain
// file. A movie timescale that cannot rescale also runs to the media end.
func editLength(e box.ElstEntry, mediaEnd uint64, movieTS, mediaTS uint32, fragmented bool) uint64 {
	start := uint64(e.MediaTime)
	end := mediaEnd
	switch {
	case e.SegmentDuration == 0:
		if !fragmented {
			return 0
		}
	case movieTS > 0:
		if seg, ok := rescaleTicks(e.SegmentDuration, movieTS, mediaTS); ok {
			if t, carry := bits.Add64(start, seg, 0); carry == 0 && t < end {
				end = t
			}
		}
	}
	if end <= start {
		return 0
	}
	return end - start
}

// setPresentation resolves the edit list and stores the run-length timing table
// and the window SeekTime and Info read. It is shared by the plain and fragmented
// paths so they cannot drift. The retained timing state is at most one 8-byte run
// per distinct stretch of equal durations: bounded by the stts entries (inside the
// capped moov) on the plain path and by the sample count on the fragmented path,
// never an array of cumulative ticks per sample.
func (rd *Reader) setPresentation(tr *track, movieTS uint32, runs []box.SttsRun, fragmented bool) {
	w := resolvePresentation(tr, movieTS, runs, fragmented)
	rd.timeRuns = runs
	rd.timescale = tr.mdhdTS
	rd.window = w
	rd.info.EncoderDelay = int64(min(w.start, math.MaxInt64))
	rd.info.Duration = ticksToDuration(w.presented, tr.mdhdTS)
	rd.info.StartOffset = ticksToDuration(w.lead, movieTS)
}

// ticksToDuration converts ticks at the given timescale (ticks per second) to a
// time.Duration, rounding down in integer arithmetic so the result is identical
// on every platform. It saturates at MaxInt64 and returns 0 for a zero timescale.
func ticksToDuration(ticks uint64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	hi, lo := bits.Mul64(ticks, uint64(time.Second))
	if hi >= uint64(timescale) {
		return math.MaxInt64 // the quotient does not fit uint64; Div64 would panic
	}
	q, _ := bits.Div64(hi, lo, uint64(timescale))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(q)
}
