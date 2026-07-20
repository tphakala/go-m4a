// SPDX-License-Identifier: MIT

package m4a

import (
	"fmt"
	"math"

	"github.com/tphakala/go-m4a/internal/box"
)

// Brands for the fragmented output, as they appear in ftyp and styp. The init
// segment declares "cmfc" (CMAF track-file conformance, ISO/IEC 23000-19) and
// lists "iso6", the ISO-BMFF version fragmented-MP4 players expect. Media segments
// declare "msdh" (a media segment) and "cmfs" (a CMAF segment); "msix" is
// deliberately absent because it asserts a sidx index this writer does not emit.
var (
	brandCMFC = box.NewFourCC("cmfc")
	brandISO6 = box.NewFourCC("iso6")
	brandISOM = box.NewFourCC("isom")
	brandMSDH = box.NewFourCC("msdh")
	brandCMFS = box.NewFourCC("cmfs")
)

// fragmentTrackID is the track_ID of the single audio track. Fragmented output
// carries exactly one track, so it is fixed rather than configurable.
const fragmentTrackID = 1

// maxSamplesPerSegment and maxSegmentBytes bound what one media segment may
// accumulate, so a caller that never flushes fails with an error instead of
// growing until the host runs out of memory. Both are far beyond any real segment:
// a two-second AAC-LC segment holds about 94 access units and some tens of
// kilobytes. The byte bound is the one that matters in practice, because a
// large-frame codec reaches a dangerous footprint long before the sample count
// looks unusual, and it also keeps every access unit comfortably inside the uint32
// the sample table records.
const (
	maxSamplesPerSegment = 1 << 20
	maxSegmentBytes      = 64 << 20
)

// InitSegment builds the CMAF initialization segment for cfg: the ftyp box and a
// moov that describes the track but contains no samples, with an mvex/trex
// declaring the movie fragmented. It is the payload an HLS playlist references
// with EXT-X-MAP, and it is constant for a given config, so a caller builds it
// once per stream.
//
// cfg is the same WriterConfig the non-fragmented Writer takes, so the codec
// selection, AudioSpecificConfig, STREAMINFO, Opus pre-skip and EncoderDelay all
// carry over unchanged. Two fields behave differently here. MediaLength has no
// meaning, because a live stream has no known total length, so a non-zero value is
// rejected rather than silently ignored. Brand still overrides the ftyp major
// brand, but the fragmented default is "cmfc" rather than "M4A ", so overriding it
// moves the CMAF declaration out of the major-brand position (it stays in the
// compatible-brand list, which is fixed); reusing a config written for the
// non-fragmented writer is the likely way to do that by accident. Brand never
// applies to the styp of a media segment, whose brands are fixed by the format.
//
// The encoder priming is trimmed with an edit list, as in the non-fragmented
// writer: EncoderDelay zero selects the codec's default priming, a positive value
// trims exactly that many samples, and NoEdit omits the edit list entirely. A
// codec with no priming at all (FLAC) also gets no edit list, since there would be
// nothing to trim. The entry uses segment_duration 0, the open-ended form used by
// fragmented-MP4 packagers for a track whose duration is not yet known; it is a
// de-facto convention rather than a normative rule, and ffmpeg's HLS fMP4 packager
// emits exactly the same (0, 0) shape.
//
// Note that ffmpeg's HLS fMP4 packager writes an edit list that trims nothing
// (media_time 0), and its plain fragmented path (-movflags +empty_moov) writes no
// edit list at all, so players vary in how much they exercise this. A caller that
// hits a player which mishandles it can set NoEdit and accept the codec's priming
// as a constant offset (about 21 ms for AAC-LC at 48 kHz).
func InitSegment(cfg WriterConfig) ([]byte, error) {
	if err := validateFragmentConfig(cfg); err != nil {
		return nil, err
	}
	meta := newTrackMeta(cfg)

	majorBrand := brandCMFC
	if cfg.Brand != "" {
		majorBrand = box.NewFourCC(cfg.Brand)
	}
	out := box.AppendFtyp(nil, majorBrand, 0, brandCMFC, brandISO6, brandISOM)

	// stbl: the sample description, then the four sample tables with no entries.
	// A fragmented track carries its samples in the fragments, but the tables must
	// still be structurally present or strict parsers (AVFoundation among them)
	// refuse to initialize the track.
	var stbl []byte
	stbl = box.AppendStsdEntry(stbl, meta.sampleEntry())
	stbl = box.AppendSttsRuns(stbl, nil)
	stbl = box.AppendStscEntries(stbl, nil)
	stbl = box.AppendStsz(stbl, nil)
	stbl = box.AppendStco(stbl, nil)

	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, stbl)

	// Every duration in the init segment is zero: the length of a live stream is
	// not known when it starts, and the fragments carry the timeline themselves.
	var mdia []byte
	mdia = box.AppendMdhd(mdia, meta.timescale, 0)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	var trak []byte
	trak = box.AppendTkhd(trak, fragmentTrackID, 0)
	// A zero trim means there is nothing to present differently from the media, so
	// emit no edit list at all. That covers both NoEdit and a codec without priming
	// (FLAC), and it matters: an entry with media_time 0 and segment_duration 0 is a
	// null edit, which a player reading segment_duration literally can take as
	// "present nothing". flacm4a suppresses the edit list on the non-fragmented path
	// for the same reason.
	if delay := meta.resolveDelay(cfg.EncoderDelay); delay != 0 {
		// segment_duration 0 is the open-ended edit: present the track from
		// media_time to its end, whenever that turns out to be.
		trak = box.AppendEdts(trak, box.AppendElst(nil, 0, int64(delay)))
	}
	trak = box.AppendMdia(trak, mdia)

	moov := box.AppendMvhd(nil, meta.timescale, 0)
	moov = box.AppendTrak(moov, trak)
	// mvex is what declares the movie fragmented. default_sample_duration is the
	// codec's fixed frame length where it has one (AAC-LC's 1024) and zero
	// otherwise, in which case each fragment states its own.
	moov = box.AppendMvex(moov, box.AppendTrex(nil,
		fragmentTrackID, meta.defaultDuration, box.SyncSampleFlags))

	return box.AppendMoov(out, moov), nil
}

// FragmentWriter emits the media segments of a fragmented MP4 (CMAF) stream: the
// styp, moof and mdat boxes that follow the InitSegment. It exists because the
// non-fragmented Writer needs an io.WriteSeeker to patch the mdat size at Close,
// which a live HLS stream cannot provide; a FragmentWriter only ever appends.
//
// Access units are buffered with WriteFrame or WriteFrameDuration and flushed as
// one segment by AppendSegment. The buffers are reused across segments, so a
// steady-state stream allocates nothing per segment once they have grown.
//
// A FragmentWriter is not safe for concurrent use. One live stream needs one
// FragmentWriter, which is also what keeps the sequence number and decode time
// monotonic.
type FragmentWriter struct {
	// Normalized codec configuration, shared with Writer.
	trackMeta

	// sequenceNumber is the mfhd sequence_number of the next segment, counting
	// from 1. baseDecodeTime is the next segment's tfdt baseMediaDecodeTime, the
	// running sum of every duration flushed so far, in the media timescale.
	sequenceNumber uint32
	baseDecodeTime uint64

	// Pending-sample arena, retained across segments. samples holds the access
	// units back to back, exactly as they will appear in mdat; sizes and durations
	// are parallel per-sample tables.
	samples   []byte
	sizes     []uint32
	durations []uint32

	// uniform tracks whether every pending duration is identical, which lets the
	// segment declare one default duration in tfhd and omit the per-sample list.
	// It holds vacuously for an empty arena.
	uniform         bool
	pendingDuration uint64
}

// NewFragmentWriter creates a FragmentWriter for cfg, validated exactly as
// InitSegment validates it. The caller is responsible for building the matching
// init segment; the two must come from the same config.
func NewFragmentWriter(cfg WriterConfig) (*FragmentWriter, error) {
	if err := validateFragmentConfig(cfg); err != nil {
		return nil, err
	}
	return &FragmentWriter{
		trackMeta:      newTrackMeta(cfg),
		sequenceNumber: 1,
		uniform:        true,
	}, nil
}

// Reset rebinds the writer to cfg and returns it to its initial state: sequence
// number 1, decode time 0, and no pending samples. The sample buffers keep their
// capacity, so a writer pooled across streams stops allocating after the first.
// Any samples buffered but not yet flushed are discarded.
func (f *FragmentWriter) Reset(cfg WriterConfig) error {
	if err := validateFragmentConfig(cfg); err != nil {
		return err
	}
	f.trackMeta = newTrackMeta(cfg)
	f.sequenceNumber = 1
	f.baseDecodeTime = 0
	f.discardPending()
	return nil
}

// WriteFrame buffers one access unit using the codec's fixed per-sample duration,
// which exists only for AAC-LC (1024 samples). Opus and FLAC frames vary in
// duration, so for those it returns an error directing the caller to
// WriteFrameDuration.
func (f *FragmentWriter) WriteFrame(au []byte) error {
	if f.defaultDuration == 0 {
		return fmt.Errorf("go-m4a: WriteFrame: %s frames vary in duration; use WriteFrameDuration", f.codec)
	}
	return f.WriteFrameDuration(au, f.defaultDuration)
}

// WriteFrameDuration buffers one access unit and records sampleDuration, the
// number of samples per channel it decodes to in the media timescale: 1024 for an
// AAC-LC access unit, the packet's 48 kHz sample count for Opus, or the block size
// for FLAC. The access unit is copied, so the caller may reuse its buffer
// immediately. It rejects an empty access unit and a zero duration.
func (f *FragmentWriter) WriteFrameDuration(au []byte, sampleDuration uint32) error {
	if len(au) == 0 {
		return fmt.Errorf("go-m4a: WriteFrameDuration: empty access unit")
	}
	if sampleDuration == 0 {
		return fmt.Errorf("go-m4a: WriteFrameDuration: sample duration must be positive")
	}
	if len(f.sizes) >= maxSamplesPerSegment {
		return fmt.Errorf("go-m4a: WriteFrameDuration: segment would exceed the limit of %d samples; call AppendSegment more often", maxSamplesPerSegment)
	}
	// Bound the pending bytes as well as the count. Rejecting the write that would
	// cross the line keeps the buffered samples flushable, where discovering the
	// problem at AppendSegment would leave the writer stuck with a segment it can
	// neither extend nor emit.
	// Subtract rather than add: on a 32-bit build the sum of two large lengths
	// could wrap negative and slip past the guard.
	if len(au) > maxSegmentBytes-len(f.samples) {
		return fmt.Errorf("go-m4a: WriteFrameDuration: segment would exceed %d bytes; call AppendSegment more often", maxSegmentBytes)
	}

	if len(f.sizes) > 0 && sampleDuration != f.durations[0] {
		f.uniform = false
	}
	f.samples = append(f.samples, au...)
	f.sizes = append(f.sizes, uint32(len(au)))
	f.durations = append(f.durations, sampleDuration)
	f.pendingDuration += uint64(sampleDuration)
	return nil
}

// PendingDuration is the total decode duration of the buffered access units, in
// the media timescale, which is what a caller cuts segments on and what the
// playlist's EXTINF reports. Read it before AppendSegment, which resets it to
// zero. Dividing by the sample rate gives seconds.
func (f *FragmentWriter) PendingDuration() uint64 { return f.pendingDuration }

// PendingSamples is the number of buffered access units.
func (f *FragmentWriter) PendingSamples() int { return len(f.sizes) }

// BaseMediaDecodeTime is the decode time the next segment will start at, in the
// media timescale: the sum of every duration flushed so far. It is the stream
// position a caller correlates with wall clock for EXT-X-PROGRAM-DATE-TIME.
func (f *FragmentWriter) BaseMediaDecodeTime() uint64 { return f.baseDecodeTime }

// AppendSegment appends one complete media segment (styp, moof and mdat) for the
// buffered access units to dst and returns the extended slice, in the manner of
// the strconv Append functions. Passing a retained buffer as dst[:0] reuses its
// capacity, which is the point: a live stream reuses one buffer per segment.
//
// It advances the sequence number and the base media decode time, and empties the
// sample buffers ready for the next segment. It returns an error if no access
// units are buffered. On any error dst is returned truncated to the length it came
// in at, so a failed call neither emits a partial segment nor loses the caller's
// buffer, and the writer's own state is left untouched.
func (f *FragmentWriter) AppendSegment(dst []byte) ([]byte, error) {
	origLen := len(dst)
	if len(f.sizes) == 0 {
		return dst[:origLen], fmt.Errorf("go-m4a: AppendSegment: no access units buffered")
	}
	// The mdat box length is a 32-bit field. maxSegmentBytes already keeps the
	// payload a factor of 64 below that, so this cannot fire today; it
	// is kept as a cheap assertion guarding the emitted box size directly, rather
	// than trusting a bound declared elsewhere to stay where it is.
	if uint64(len(f.samples))+box.MdatShortHeaderSize > math.MaxUint32 {
		return dst[:origLen], fmt.Errorf("go-m4a: AppendSegment: %d bytes of samples overflow the mdat box size", len(f.samples))
	}

	// trun's data_offset is measured from the first byte of the enclosing moof, so
	// the moof length must be known before any of it is written. Every box in a
	// fragment has a fixed or exactly predictable size, so compute it up front and
	// keep the byte stream purely sequential.
	var defaultDuration uint32
	var trunDurations []uint32
	if f.uniform {
		defaultDuration = f.durations[0]
	} else {
		trunDurations = f.durations
	}
	trafSize := box.TrafHeaderSize + box.TfhdSize(defaultDuration) + box.TfdtSize +
		box.TrunSize(len(f.sizes), trunDurations != nil)
	moofSize := box.MoofHeaderSize + box.MfhdSize + trafSize
	// Widen before adding: on a 32-bit build this sum done in int could wrap to a
	// negative and slip past the check below, which is the one thing that must not
	// happen to the offset every player uses to find the sample data. The sample
	// caps keep the real value near 8 MB, so the check is an assertion rather than a
	// reachable path.
	dataOffset := int64(moofSize) + box.MdatShortHeaderSize
	if dataOffset > math.MaxInt32 {
		return dst[:origLen], fmt.Errorf("go-m4a: AppendSegment: moof of %d bytes overflows the trun data offset", moofSize)
	}

	dst = box.AppendStyp(dst, brandMSDH, 0, brandMSDH, brandCMFS)

	// Emit the fragment straight into dst. Every box size is already known, so the
	// children go in directly after each container header instead of being
	// assembled in a temporary buffer and copied; that is what makes a steady-state
	// segment allocation-free.
	moofStart := len(dst)
	dst = box.AppendMoofHeader(dst, uint32(moofSize))
	dst = box.AppendMfhd(dst, f.sequenceNumber)
	dst = box.AppendTrafHeader(dst, uint32(trafSize))
	dst = box.AppendTfhd(dst, fragmentTrackID, defaultDuration)
	dst = box.AppendTfdt(dst, f.baseDecodeTime)
	dst = box.AppendTrun(dst, int32(dataOffset), f.sizes, trunDurations)

	// The predicted length is what every data_offset in the segment was computed
	// from. If a box layout ever drifts from its predictor the offsets silently
	// point into the wrong bytes, so fail loudly instead.
	if got := len(dst) - moofStart; got != moofSize {
		return dst[:origLen], fmt.Errorf("go-m4a: AppendSegment: internal error: moof is %d bytes, predicted %d", got, moofSize)
	}

	dst = box.AppendMdat(dst, f.samples)

	// The moof check above stops at the mdat header, but data_offset spans it too.
	// AppendMdat builds that header through a shared container helper, so a future
	// largesize path there would shift every sample by eight bytes while the moof
	// check still passed. Verify the offset against where the payload actually
	// landed, which is the property players depend on.
	if got := int64(len(dst) - moofStart - len(f.samples)); got != dataOffset {
		return dst[:origLen], fmt.Errorf("go-m4a: AppendSegment: internal error: sample data starts %d bytes into the fragment, predicted %d", got, dataOffset)
	}

	f.sequenceNumber++
	f.baseDecodeTime += f.pendingDuration
	f.discardPending()
	return dst, nil
}

// discardPending empties the sample arena while keeping its capacity for the next
// segment.
func (f *FragmentWriter) discardPending() {
	f.samples = f.samples[:0]
	f.sizes = f.sizes[:0]
	f.durations = f.durations[:0]
	f.uniform = true
	f.pendingDuration = 0
}

// validateFragmentConfig applies the shared WriterConfig validation, then rejects
// the one field that cannot mean anything for a fragmented stream.
func validateFragmentConfig(cfg WriterConfig) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if cfg.MediaLength != 0 {
		// MediaLength pins the non-fragmented edit list to an exact source length,
		// which a live stream of unknown duration has no equivalent of. Reject it
		// rather than accept a value that would be quietly dropped.
		return fmt.Errorf("go-m4a: MediaLength %d is not supported for fragmented output; leave it zero", cfg.MediaLength)
	}
	return nil
}
