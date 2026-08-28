// SPDX-License-Identifier: MIT

package m4a

import (
	"fmt"

	"github.com/tphakala/go-m4a/internal/box"
)

// Box type codes the fragmented demux path matches while walking movie fragments.
// The plain-path codes (moof, mvhd, mvex, ...) live in reader.go's var block.
var (
	fourccTraf = box.NewFourCC("traf")
	fourccTfhd = box.NewFourCC("tfhd")
	fourccTfdt = box.NewFourCC("tfdt")
	fourccTrun = box.NewFourCC("trun")
	fourccTrex = box.NewFourCC("trex")
)

// parseFragmented builds the Reader from a fragmented (CMAF) stream: the init
// segment's moov (metadata, no sample tables) plus the movie fragments described
// by moofs. It selects the same first supported "soun" track the plain path
// would, fills Info from the sample entry, then walks every fragment's
// tfhd/trun to lay out the shared per-chunk geometry the read path consumes.
func (rd *Reader) parseFragmented(moov []byte, moofs []moofExtent) error {
	tr, movieTS, movieDur, trex, err := parseInitMoov(moov)
	if err != nil {
		return err
	}
	if err := rd.validateCodec(tr); err != nil {
		return err
	}
	rd.fillFormatInfo(tr, movieTS, movieDur)
	if err := rd.buildFragmentGeometry(moofs, tr, trex); err != nil {
		return err
	}
	rd.info.FrameCount = rd.sampleCount
	return nil
}

// parseInitMoov walks the init segment's moov like parseMoov, but tolerates the
// mvex box (which declares the movie fragmented) instead of rejecting it, and
// ignores the structurally-present but empty sample tables. It returns the chosen
// track, the movie timescale and duration, and the trex defaults matching the
// chosen track's track_ID (a zero Trex when the init segment carries none).
func parseInitMoov(moov []byte) (tr *track, movieTS uint32, movieDur uint64, trex box.Trex, err error) {
	var chosen *track
	var sawSoun bool
	trexByID := make(map[uint32]box.Trex)

	walkErr := box.WalkChildren(moov, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccMvhd:
			ts, dur, perr := box.ParseMvhd(body)
			if perr != nil {
				return perr
			}
			movieTS, movieDur = ts, dur
		case fourccMvex:
			return box.WalkChildren(body, func(t box.FourCC, b []byte) error {
				if t != fourccTrex {
					return nil
				}
				tx, perr := box.ParseTrex(b)
				if perr != nil {
					return perr
				}
				trexByID[tx.TrackID] = tx
				return nil
			})
		case fourccTrak:
			return selectSounTrack(body, &chosen, &sawSoun)
		}
		return nil
	})
	if walkErr != nil {
		return nil, 0, 0, box.Trex{}, wrapParse(walkErr)
	}
	if chosen == nil {
		return nil, 0, 0, box.Trex{}, errNoSupportedSoun(sawSoun)
	}
	return chosen, movieTS, movieDur, trexByID[chosen.trackID], nil
}

// buildFragmentGeometry walks every movie fragment and lays out the shared
// per-chunk geometry (sizes, chunk offsets, samples-per-chunk) that the read path
// consumes, modeling each trun run as one contiguous chunk. It reuses the plain
// path's validateOffsets to confirm every access unit lies within the stream, and
// sets Info.Duration from the summed sample durations at the media timescale.
func (rd *Reader) buildFragmentGeometry(moofs []moofExtent, tr *track, trex box.Trex) error {
	var (
		sizes        []uint32
		totalSamples int64
		totalDur     uint64
	)
	// One chunk per movie fragment is the common CMAF layout, so size to the
	// fragment count; a fragment carrying several trun runs or none makes this an
	// estimate, which is fine for a capacity hint.
	chunkOffsets := make([]int64, 0, len(moofs))
	perChunk := make([]int64, 0, len(moofs))
	acc := &fragAccumulator{
		streamLen: rd.streamLen,
		track:     tr,
		trex:      trex,
		sizes:     &sizes,
		chunks:    &chunkOffsets,
		perChunk:  &perChunk,
		samples:   &totalSamples,
		duration:  &totalDur,
	}

	for _, ext := range moofs {
		// scanTopLevel already parsed and bounds-checked this moof's header, so
		// read the body directly from the cached extent instead of re-reading and
		// re-parsing the 16-byte header per fragment.
		moofBody, err := readSection(rd.r, ext.bodyStart, ext.bodyLen)
		if err != nil {
			return fmt.Errorf("go-m4a: read moof body at %d: %w", ext.offset, err)
		}
		acc.moofOffset = ext.offset
		err = box.WalkChildren(moofBody, func(typ box.FourCC, body []byte) error {
			if typ != fourccTraf {
				return nil
			}
			return acc.addTraf(body)
		})
		if err != nil {
			return wrapParse(err)
		}
	}

	if totalSamples > maxInt {
		return fmt.Errorf("go-m4a: fragmented sample count %d exceeds addressable memory: %w", totalSamples, ErrCorrupt)
	}
	// The stream carried moof boxes (that is why this path runs), so no sample
	// matching the selected track means the fragments belong to another track or
	// the init segment's tkhd was missing and the track_ID never bound. Since
	// addTraf stopped validating foreign trafs, a stream whose only trafs are
	// foreign and corrupt also lands here rather than on the corruption. Reject it
	// rather than hand back a reader that reports zero frames, which a caller cannot
	// tell apart from a genuine track-binding failure.
	if totalSamples == 0 {
		return fmt.Errorf("go-m4a: no movie fragment carries samples for track_ID %d: %w", tr.trackID, ErrCorrupt)
	}
	rd.sizes = sizes
	rd.sampleCount = int(totalSamples)
	rd.chunkOffsets = chunkOffsets
	rd.samplesPerChunk = perChunk
	if err := rd.validateOffsets(); err != nil {
		return err
	}
	// The init segment's durations are all zero, so presentationDuration reported
	// zero; the real presentation length is the sum of the fragments' sample
	// durations at the media timescale.
	if tr.mdhdTS > 0 && totalDur > 0 {
		rd.info.Duration = ticksToDuration(totalDur, tr.mdhdTS)
	}
	return nil
}

// fragAccumulator threads the growing geometry and the fixed per-track context
// through the traf walk, so addTraf stays a method with a small signature rather
// than a function taking a dozen pointers.
type fragAccumulator struct {
	streamLen  int64
	track      *track
	trex       box.Trex
	moofOffset int64

	sizes    *[]uint32
	chunks   *[]int64
	perChunk *[]int64
	samples  *int64
	duration *uint64
}

// addTraf decodes one track fragment: it reads the tfhd, binds the fragment to
// the selected track, then resolves the base file offset its data offsets are
// measured from and appends every run's samples to the geometry. A run with no
// data_offset continues immediately after the previous run's data, per
// ISO/IEC 14496-12 8.8.8.
//
// The traf is walked twice: once for the tfhd alone, and again for the runs, only
// once the tfhd has bound the fragment to this track. A moof in a muxed stream
// carries a traf per track, and reading a video track's boxes to discard them is
// work this demuxer never needs.
//
// Two things pay for the extra walk, and the smaller one is the obvious one.
// Skipping a foreign traf saves its ParseTrun and ParseTfdt, which is a fixed
// cost per traf rather than a per-sample one: ParseTrun bounds-checks and slices
// and does not decode records, so it runs in the same dozen nanoseconds for a
// run of one sample and a run of a million. That saving scales with how many
// foreign trafs a moof carries, which for this package's own output is none.
//
// What the two-pass form actually buys on that output is an allocation. Deciding
// the track before the runs are read means each run can be handed to addRun as it
// is parsed; a single pass has to buffer the runs in a []box.Trun until the tfhd
// is known, and that slice is one allocation per traf. Measured on
// BenchmarkOpenFragmented against a single-pass version of this function, it is
// 534 allocations against 434 over 100 fragments, with the wall time a wash. So
// the walk is not free and this benchmark credits none of the skip, yet the trade
// is still worth making on the one shape the package emits itself.
//
// Skipping a foreign traf before its body is read means its tfdt and trun are no
// longer structurally validated: a malformed one in a video traf used to fail the
// open and now does not. That is the intended contract. This demuxer extracts one
// audio track, and another track's internal framing is not its business to
// police. What guards this reader's own arithmetic is unaffected: a traf still
// needs a parsable tfhd to bind at all, and the framing of every child box in
// every traf is still checked, because trafHeader walks the traf to the end
// rather than stopping at the tfhd.
//
// Two things did change for the selected track, both narrow. Its base offset is
// now resolved before its runs are parsed, so a traf that both lacks a base and
// carries a malformed tfdt or trun reports the missing base (ErrUnsupported)
// where it used to report the malformed box (ErrCorrupt). And a run's geometry is
// now built as the run is read rather than after every run in the traf has been
// parsed, so among several runs the first geometry error wins where a later
// parse error used to. Both were errors before and are errors now; only which one
// surfaces changed.
func (a *fragAccumulator) addTraf(traf []byte) error {
	tfhd, haveTfhd, err := trafHeader(traf)
	if err != nil {
		return err
	}
	if !haveTfhd {
		return fmt.Errorf("go-m4a: traf without tfhd: %w", ErrCorrupt)
	}
	// Bind fragments to the selected track by track_ID; a moof may also carry a
	// video track's traf, which is silently skipped.
	if tfhd.TrackID != a.track.trackID {
		return nil
	}

	// duration-is-empty declares a fragment with no samples. Such a traf resolves
	// no base offset and contributes nothing, but its boxes are still parsed by the
	// walk below, which keeps the validation the single-pass version gave them. The
	// two halves of that decision are the guard here and the early return in the
	// trun case; they have to agree, and this is why base is left at 0 on the path
	// that never reads it.
	var base int64
	if !tfhd.DurationIsEmpty {
		if base, err = a.resolveBase(tfhd); err != nil {
			return err
		}
	}
	dataCursor := base
	return box.WalkChildren(traf, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccTfdt:
			// Parsed to validate its structure; the decode time is not needed for
			// access-unit extraction, which derives every offset from the trun runs.
			if _, perr := box.ParseTfdt(body); perr != nil {
				return perr
			}
		case fourccTrun:
			tn, perr := box.ParseTrun(body)
			if perr != nil {
				return perr
			}
			// Parsed for its structure, then dropped: an empty fragment declares no
			// samples, so the run contributes none. See the base guard above.
			if tfhd.DurationIsEmpty {
				return nil
			}
			end, aerr := a.addRun(&tn, base, dataCursor, tfhd)
			if aerr != nil {
				return aerr
			}
			dataCursor = end
		}
		return nil
	})
}

// trafHeader walks a traf for its tfhd alone and decodes it, so a caller can
// decide whether the fragment belongs to the selected track before spending
// anything on the rest of the box.
//
// It deliberately walks the traf to the end instead of stopping once it has the
// tfhd. Completing the walk is what keeps every child box's framing checked even
// in a traf whose body is otherwise skipped, so an early exit would trade a
// header scan for a hole in the validation of foreign fragments.
//
// Absence of a tfhd is reported as found == false rather than as an error,
// keeping "this traf has no header" separate from "this traf has a header that
// will not parse". The caller currently rejects both, but with different messages,
// and folding the two would lose the distinction at the point where it is known.
func trafHeader(traf []byte) (tfhd box.Tfhd, found bool, err error) {
	err = box.WalkChildren(traf, func(typ box.FourCC, body []byte) error {
		if typ != fourccTfhd {
			return nil
		}
		t, perr := box.ParseTfhd(body)
		if perr != nil {
			return perr
		}
		tfhd, found = t, true
		return nil
	})
	if err != nil {
		return box.Tfhd{}, false, err
	}
	return tfhd, found, nil
}

// resolveBase returns the file offset the fragment's data offsets are measured
// from. CMAF mandates default-base-is-moof, which makes a segment relocatable; an
// explicit base_data_offset is also honored. A traf with neither is rejected: its
// base would be the end of the previous track's data, which this single-track
// demuxer does not track across skipped (video) fragments.
func (a *fragAccumulator) resolveBase(tfhd box.Tfhd) (int64, error) {
	switch {
	case tfhd.HasBaseDataOffset:
		if tfhd.BaseDataOffset > 1<<62 {
			return 0, fmt.Errorf("go-m4a: tfhd base_data_offset %d too large: %w", tfhd.BaseDataOffset, ErrCorrupt)
		}
		return int64(tfhd.BaseDataOffset), nil
	case tfhd.DefaultBaseIsMoof:
		return a.moofOffset, nil
	default:
		return 0, fmt.Errorf("go-m4a: traf without base_data_offset or default-base-is-moof: %w", ErrUnsupported)
	}
}

// addRun appends one trun's samples to the geometry and returns the file offset
// one past the run's data (the implicit base for a following run without its own
// data_offset). A run without data_offset starts at dataCursor; one with it starts
// at base+data_offset. Each sample's size and duration are resolved against the
// tfhd and trex defaults when the trun omits them.
func (a *fragAccumulator) addRun(tn *box.Trun, base, dataCursor int64, tfhd box.Tfhd) (int64, error) {
	runStart := dataCursor
	if tn.HasDataOffset {
		runStart = base + int64(tn.DataOffset)
	}
	if runStart < 0 {
		return 0, fmt.Errorf("go-m4a: trun data offset %d yields negative position: %w", tn.DataOffset, ErrCorrupt)
	}
	// Each access unit occupies at least one byte of the stream and real samples do
	// not overlap, so the running sample total cannot exceed the stream length.
	// Bounding it here caps the geometry slices before the per-sample loop grows
	// them, so a hostile sample_count cannot exhaust memory.
	newTotal := *a.samples + int64(tn.SampleCount)
	if newTotal > a.streamLen {
		return 0, fmt.Errorf("go-m4a: fragment sample count exceeds stream %d: %w", a.streamLen, ErrCorrupt)
	}
	// On a 32-bit build maxInt (math.MaxInt32) sits below streamLen for a stream
	// over 2 GiB, so the stream-length bound alone would let the geometry slices
	// grow past the addressable element count and panic in append mid-walk. Reject
	// here, before the loop, exactly as the plain path's buildGeometry does for
	// stsz. On a 64-bit build maxInt is enormous, so this never fires.
	if newTotal > maxInt {
		return 0, fmt.Errorf("go-m4a: fragment sample count %d exceeds addressable memory: %w", newTotal, ErrCorrupt)
	}
	if tn.SampleCount == 0 {
		return runStart, nil
	}

	*a.chunks = append(*a.chunks, runStart)
	*a.perChunk = append(*a.perChunk, int64(tn.SampleCount))

	runBytes := int64(0)
	for i := 0; i < int(tn.SampleCount); i++ {
		size, ok := resolveSampleSize(tn, i, tfhd, a.trex)
		if !ok || size == 0 {
			return 0, fmt.Errorf("go-m4a: trun sample %d has zero or unresolved size: %w", i, ErrCorrupt)
		}
		*a.sizes = append(*a.sizes, size)
		runBytes += int64(size)
		*a.duration += uint64(resolveSampleDuration(tn, i, tfhd, a.trex))
	}
	*a.samples += int64(tn.SampleCount)
	return runStart + runBytes, nil
}

// resolveSampleSize returns sample i's byte length, preferring the per-sample size
// in the trun, then the tfhd default, then the trex default. ok is false when no
// source supplies a size, which the caller treats as corrupt.
func resolveSampleSize(tn *box.Trun, i int, tfhd box.Tfhd, trex box.Trex) (uint32, bool) {
	if tn.HasSampleSize {
		return tn.SampleSize(i), true
	}
	if tfhd.HasDefaultSampleSize {
		return tfhd.DefaultSampleSize, true
	}
	if trex.DefaultSampleSize != 0 {
		return trex.DefaultSampleSize, true
	}
	return 0, false
}

// resolveSampleDuration returns sample i's duration in media-timescale ticks,
// preferring the per-sample duration in the trun, then the tfhd default, then the
// trex default (0 when none is present, which leaves Info.Duration unset).
func resolveSampleDuration(tn *box.Trun, i int, tfhd box.Tfhd, trex box.Trex) uint32 {
	if tn.HasSampleDuration {
		return tn.SampleDuration(i)
	}
	if tfhd.HasDefaultSampleDuration {
		return tfhd.DefaultSampleDuration
	}
	return trex.DefaultSampleDuration
}
