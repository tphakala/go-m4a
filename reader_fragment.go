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
	// the init segment's tkhd was missing and the track_ID never bound. Reject it
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

// addTraf decodes one track fragment: it reads the tfhd and every trun, skips
// fragments for other tracks (a video traf) and empty fragments, resolves each
// run's base file offset, and appends each run's samples to the geometry. A run
// with no data_offset continues immediately after the previous run's data, per
// ISO/IEC 14496-12 8.8.8.
func (a *fragAccumulator) addTraf(traf []byte) error {
	var (
		tfhd     box.Tfhd
		haveTfhd bool
		truns    []box.Trun
	)
	err := box.WalkChildren(traf, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccTfhd:
			t, perr := box.ParseTfhd(body)
			if perr != nil {
				return perr
			}
			tfhd, haveTfhd = t, true
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
			truns = append(truns, tn)
		}
		return nil
	})
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
	// duration-is-empty declares a fragment with no samples.
	if tfhd.DurationIsEmpty {
		return nil
	}

	base, err := a.resolveBase(tfhd)
	if err != nil {
		return err
	}
	dataCursor := base
	for i := range truns {
		end, err := a.addRun(&truns[i], base, dataCursor, tfhd)
		if err != nil {
			return err
		}
		dataCursor = end
	}
	return nil
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
	if tn.HasSampleSize && tn.Samples != nil {
		return tn.Samples[i].Size, true
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
	if tn.HasSampleDuration && tn.Samples != nil {
		return tn.Samples[i].Duration
	}
	if tfhd.HasDefaultSampleDuration {
		return tfhd.DefaultSampleDuration
	}
	return trex.DefaultSampleDuration
}
