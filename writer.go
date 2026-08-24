// SPDX-License-Identifier: MIT

// Package m4a muxes AAC-LC, Opus, or FLAC access units into an MP4/M4A container
// and demuxes them back out. It is the container half that codecs like go-aac
// deliberately leave to an external muxer: an edit list (elst) trims the encoder
// priming so the written file is sample-accurate and gapless.
//
// There are two writers. Writer produces a plain ftyp|mdat|moov file and needs an
// io.WriteSeeker, because the mdat size is patched at Close. InitSegment and
// FragmentWriter produce fragmented (CMAF) output for live HLS or DASH: they only
// ever append, so they can write to a byte slice or a socket. Reading is
// non-fragmented only, so the package writes a shape Reader deliberately rejects
// with ErrUnsupported.
//
// The public surface is stdlib-only; the codec bridges (aacm4a, opusm4a, flacm4a)
// are optional subpackages. The ISO-BMFF byte mechanics live in the internal/box
// package.
package m4a

import (
	"fmt"
	"io"
	"math"

	"github.com/tphakala/go-m4a/internal/box"
)

// DefaultEncoderDelay is the number of leading priming samples an AAC-LC encoder
// emits before the first real sample. It is go-aac's measured low-level encoder
// priming (one 1024-sample frame) and is the value used when WriterConfig leaves
// EncoderDelay at zero.
const DefaultEncoderDelay = 1024

// NoEdit is the WriterConfig.EncoderDelay sentinel that suppresses the edit list
// entirely: the writer emits no edts/elst and presents every decoded sample.
const NoEdit = -1

// DefaultOpusPreSkip is the pre-skip an Opus encoder emits before the first real
// sample, in samples at the 48 kHz Opus timescale. It is go-opus's measured value
// (Encoder.PreSkip) and the value used when a CodecOpus WriterConfig leaves
// OpusPreSkip at zero.
const DefaultOpusPreSkip = 312

// opusTimescale is the media (and, here, movie) timescale for an Opus track. The
// Encapsulation of Opus in ISOBMFF fixes it at 48000 regardless of the original
// input rate, so a CodecOpus writer requires SampleRate == 48000 and every sample
// duration is counted in 48 kHz samples.
const opusTimescale = 48000

// samplesPerFrame is the fixed AAC-LC output length of one access unit, in
// samples per channel. Every AU decodes to exactly this many samples, so the
// stts table is a single (FrameCount, 1024) run.
const samplesPerFrame = 1024

// audioObjectTypeAACLC is the MPEG-4 Audio Object Type for AAC-LC, as it appears
// in the first five bits of an AudioSpecificConfig.
const audioObjectTypeAACLC = 2

// flacStreamInfoLen is the exact size of a FLAC STREAMINFO metadata block, the
// dfLa box payload. It is fixed by the format (RFC 9639 section 8.2), so both the
// NewWriter config check and the SetSTREAMINFO setter require exactly this length.
const flacStreamInfoLen = 34

// maxAudioSampleEntryRate is the largest integer sample rate the AudioSampleEntry
// samplerate field, a 16.16 fixed-point value, can hold: a higher rate would wrap
// when shifted into the 16-bit integer part.
//
// It is not a ceiling every codec shares. Each codec's validator decides how to
// treat a rate above it. AAC-LC rejects such rates (validateAACConfig), because
// 88200 and 96000 are sampling-frequency-table rates that do not fit and AAC
// carries no authoritative rate anywhere else, so a fallback would misreport the
// track. Opus never approaches it, its rate being pinned to opusTimescale. FLAC
// accepts rates up to maxFLACSampleRate: the fLaC sample entry then carries the
// reduced power-of-two hint from flacSampleEntryRate, while the true rate rides in
// the STREAMINFO block (which the reader reads back) and in the mdhd/mvhd
// timescale. See TestAcceptedSampleRates, which pins all three.
const maxAudioSampleEntryRate = 0xFFFF

// maxFLACSampleRate is the largest sample rate a FLAC stream can declare. The
// STREAMINFO sample-rate field is 20 bits (RFC 9639 section 8.2), so the format
// caps a stream at 0xFFFFF = 1048575 Hz. validateFLACConfig accepts up to this;
// rates above maxAudioSampleEntryRate cannot be held exactly by the sample entry,
// so the fLaC entry carries a reduced hint and the reader recovers the true rate
// from STREAMINFO.
const maxFLACSampleRate = 0xFFFFF

// maxFrames caps the number of access units per file so the moov sample tables
// and their enclosing box sizes cannot overflow the 32-bit box size field. At
// this bound the stsz box alone is about 2 GiB, well under the 4 GiB ceiling,
// and it corresponds to roughly 149 hours of 48 kHz audio, far beyond any real
// clip. Reaching it returns an error instead of silently corrupting the file.
const maxFrames = 1 << 29

// samplingFrequencyTable maps an AudioSpecificConfig samplingFrequencyIndex to a
// sample rate in Hz (ISO/IEC 14496-3 Table 1.16). Index 15 (explicit rate) is
// out of v1 scope and simply falls off the end of the table.
var samplingFrequencyTable = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

// WriterConfig configures a Writer, and also the fragmented writers InitSegment
// and NewFragmentWriter. SampleRate and Channels must agree with ASC; the
// constructors validate them against it and refuse a mismatch. Two fields mean
// something different on the fragmented path, noted on the fields themselves:
// MediaLength is rejected there, and Brand has a different default.
type WriterConfig struct {
	// Codec selects the audio codec. The zero value is CodecAACLC, so a config
	// that sets only ASC keeps muxing AAC-LC. For CodecOpus set OpusPreSkip and
	// OpusInputSampleRate (and SampleRate must be 48000); for CodecFLAC set
	// STREAMINFO.
	Codec Codec

	// SampleRate is the audio sample rate in Hz (for example 48000). Required and
	// positive; the accepted range is per codec. For AAC-LC it must be one of the
	// eleven MPEG-4 sampling-frequency table rates that fit the 16.16 sample-entry
	// field, 7350 to 64000, and must match the rate encoded in ASC. For Opus it must
	// be 48000, the fixed Opus container timescale. For FLAC any positive rate up to
	// maxFLACSampleRate (the 20-bit STREAMINFO maximum, 1048575) is accepted, well
	// past the 65535 the sample entry can hold: a higher rate is written as a reduced
	// power-of-two hint in the sample entry, while the true rate rides in STREAMINFO
	// and the media timescale.
	//
	// For FLAC, SampleRate should agree with the rate inside STREAMINFO. NewWriter
	// does not compare them, so keeping them in step is the caller's job; the value
	// that matters on read is STREAMINFO's, which this package's Reader and a
	// conforming decoder both take as authoritative (Xiph, Encapsulation of FLAC in
	// ISOBMFF). A config where they disagree produces a file whose reported rate
	// comes from STREAMINFO, not from this field.
	SampleRate int

	// Channels is the channel count, 1 (mono) or 2 (stereo). Required, and for
	// AAC-LC it must match the channel configuration encoded in ASC.
	Channels int

	// ASC is the MPEG-4 AudioSpecificConfig (two bytes for AAC-LC). Required for
	// CodecAACLC; ignored otherwise. The writer copies the bytes verbatim into the
	// esds DecoderSpecificInfo.
	ASC []byte

	// OpusPreSkip is the Opus pre-skip in samples at 48 kHz (go-opus's
	// Encoder.PreSkip). It fills the dOps PreSkip field and the edit-list media
	// time. Used only for CodecOpus; zero selects DefaultOpusPreSkip.
	OpusPreSkip int

	// OpusInputSampleRate is the original source sample rate recorded in the dOps
	// InputSampleRate field (informational; Opus always decodes at 48 kHz). Used
	// only for CodecOpus; zero selects SampleRate.
	OpusInputSampleRate int

	// STREAMINFO is the 34-byte FLAC STREAMINFO metadata block, the payload of the
	// dfLa box (from go-flac's pcm.FrameEncoder.StreamInfoBytes). Used only for
	// CodecFLAC; ignored otherwise. It may be left empty here and supplied later
	// with Writer.SetSTREAMINFO, before Close, which is what lets an encoder stream
	// frames first and provide the finalized block (with its measured frame sizes
	// and MD5) at the end. Whether given here or later, a CodecFLAC track must have
	// a 34-byte block by Close, which errors otherwise.
	STREAMINFO []byte

	// EncoderDelay is the number of leading priming samples to trim with an edit
	// list. Zero uses the codec's default priming (DefaultEncoderDelay, 1024, for
	// AAC-LC; the resolved OpusPreSkip, default 312, for Opus; 0 for FLAC); NoEdit
	// writes no edit list at all; a positive value trims exactly that many samples.
	EncoderDelay int

	// MediaLength, when greater than zero, is the number of PCM samples per
	// channel the source contained. It sets the edit-list segment duration
	// exactly, so trailing final-frame padding is also excluded. Zero presents
	// every decoded sample after the priming. A live fragmented stream has no
	// known total length, so InitSegment and NewFragmentWriter reject any non-zero
	// value rather than ignore it.
	MediaLength int64

	// Brand overrides the ftyp major brand. When set it must be exactly four bytes
	// (space-padded, for example "mp42"); the constructors reject any other
	// length. NewWriter defaults it to "M4A " and always lists "M4A ", "mp42" and
	// "isom" as compatible brands. InitSegment defaults it to "cmfc" and lists
	// "cmfc", "iso6" and "isom" instead, so overriding it there moves the CMAF
	// declaration out of the major-brand position, though it stays in the
	// compatible-brand list; it never affects a media segment's styp.
	Brand string
}

// Writer streams AAC-LC, Opus, or FLAC access units into an MP4/M4A file
// (selected by WriterConfig.Codec, defaulting to AAC-LC). The on-disk layout is
// ftyp | mdat | moov: ftyp and the mdat header are written up front, each
// WriteFrame (or WriteFrameDuration) appends one access unit to the mdat payload,
// and Close patches the mdat size and writes the moov metadata. It requires an
// io.WriteSeeker because the mdat size is a placeholder patched once at Close.
type Writer struct {
	w io.WriteSeeker

	// Normalized codec configuration, shared with FragmentWriter.
	trackMeta

	encoderDelay int
	mediaLength  int64

	// stts bookkeeping. While every access unit shares one decode duration (AAC-LC,
	// and Opus with fixed-size packets), only sampleDelta is kept and durations stays
	// nil, so Close emits a single stts run with no per-frame allocation, matching the
	// pre-generalization AAC path byte-for-byte. durations is materialized (backfilled
	// from sampleDelta) only when a WriteFrameDuration value diverges, as FLAC's short
	// final frame does, and then holds every unit's duration in write order.
	sampleDelta uint32
	durations   []uint32

	// Byte bookkeeping. mdatBoxOffset is where the mdat box header starts;
	// payloadStart is where the first access unit begins; totalPayload is the
	// running sum of access-unit lengths.
	mdatBoxOffset int64
	payloadStart  int64
	totalPayload  int64

	sizes []uint32 // per-access-unit byte lengths, in write order (for stsz)

	// State machine. writeErr latches a failed WriteFrame so the sample table
	// can never disagree with the bytes on disk; closed is set once Close is
	// called (rejecting further WriteFrame); finalized is set only after moov is
	// written, so a Close whose finalize step fails transiently can be retried.
	writeErr  error
	closed    bool
	finalized bool
}

// NewWriter validates cfg for its codec (the AAC-LC ASC against SampleRate and
// Channels, the Opus rate, or the FLAC STREAMINFO), then writes the ftyp box and
// the placeholder mdat header to w. It returns an error, prefixed "go-m4a: ", when
// the writer is nil, the codec configuration is malformed or disagrees with
// SampleRate or Channels, the sample rate is unsupported, or an initial write fails.
func NewWriter(w io.WriteSeeker, cfg WriterConfig) (*Writer, error) {
	if w == nil {
		return nil, fmt.Errorf("go-m4a: nil writer")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	majorBrand := box.NewFourCC("M4A ")
	if cfg.Brand != "" {
		majorBrand = box.NewFourCC(cfg.Brand)
	}

	// ftyp first, then the 64-bit largesize mdat header with a placeholder size.
	ftyp := box.AppendFtyp(nil, majorBrand, 0,
		box.NewFourCC("M4A "), box.NewFourCC("mp42"), box.NewFourCC("isom"))
	if _, err := w.Write(ftyp); err != nil {
		return nil, fmt.Errorf("go-m4a: write ftyp: %w", err)
	}
	if _, err := w.Write(box.AppendMdatHeader(nil)); err != nil {
		return nil, fmt.Errorf("go-m4a: write mdat header: %w", err)
	}

	return &Writer{
		w:             w,
		trackMeta:     newTrackMeta(cfg),
		encoderDelay:  cfg.EncoderDelay,
		mediaLength:   cfg.MediaLength,
		mdatBoxOffset: int64(len(ftyp)),
		payloadStart:  int64(len(ftyp) + box.MdatHeaderSize),
	}, nil
}

// trackMeta is the normalized, codec-specific configuration of the single audio
// track, captured from a WriterConfig so a later mutation of the caller's config
// or byte slices cannot change the output. Both Writer and FragmentWriter embed
// it, so the sample entry and the codec's priming defaults are built one way for
// the non-fragmented and fragmented paths alike.
type trackMeta struct {
	codec         Codec
	sampleRate    uint32
	channels      uint16
	asc           []byte // AAC-LC AudioSpecificConfig
	streamInfo    []byte // FLAC STREAMINFO (dfLa payload)
	opusPreSkip   uint16
	opusInputRate uint32

	// timescale is the media and movie timescale: SampleRate for AAC and FLAC,
	// 48000 for Opus. defaultDelay is the codec's priming trim when EncoderDelay is
	// left at zero. defaultDuration is the per-sample duration WriteFrame records
	// for a fixed-duration codec (1024 for AAC-LC); it is zero for Opus and FLAC,
	// whose callers must use WriteFrameDuration.
	timescale       uint32
	defaultDelay    int
	defaultDuration uint32
}

// newTrackMeta normalizes cfg for its codec. cfg must already have passed
// validateConfig, which guarantees the narrowing conversions here cannot wrap.
func newTrackMeta(cfg WriterConfig) trackMeta {
	m := trackMeta{
		codec:      cfg.Codec,
		sampleRate: uint32(cfg.SampleRate),
		channels:   uint16(cfg.Channels),
		timescale:  uint32(cfg.SampleRate),
	}
	switch cfg.Codec {
	case CodecAACLC:
		m.asc = append([]byte(nil), cfg.ASC...)
		m.defaultDelay = DefaultEncoderDelay // 1024
		m.defaultDuration = samplesPerFrame  // every AAC-LC AU is 1024 samples
	case CodecOpus:
		// The Opus timescale is fixed at 48000; validateConfig enforced SampleRate.
		m.opusPreSkip = uint16(cfg.OpusPreSkip)
		if m.opusPreSkip == 0 {
			m.opusPreSkip = DefaultOpusPreSkip
		}
		m.opusInputRate = uint32(cfg.OpusInputSampleRate)
		if m.opusInputRate == 0 {
			m.opusInputRate = uint32(cfg.SampleRate)
		}
		m.defaultDelay = int(m.opusPreSkip)
		// Opus packet durations vary (the final packet may be short), so callers
		// supply each with WriteFrameDuration; defaultDuration stays 0.
	case CodecFLAC:
		m.streamInfo = append([]byte(nil), cfg.STREAMINFO...)
		m.defaultDelay = 0 // FLAC has no encoder priming
		// FLAC block sizes vary (the final frame is short); callers supply each
		// with WriteFrameDuration; defaultDuration stays 0.
	}
	return m
}

// WriteFrame appends one access unit to the mdat payload using the codec's fixed
// per-sample duration. It works for AAC-LC, whose every AU is 1024 samples. For
// Opus and FLAC, whose frames vary in duration, it returns an error directing the
// caller to WriteFrameDuration. It rejects a nil or empty access unit, and any
// call after Close.
func (w *Writer) WriteFrame(au []byte) error {
	if w.defaultDuration == 0 {
		// Opus and FLAC frames vary in duration, so there is no fixed value
		// WriteFrame can supply; the caller must use WriteFrameDuration.
		return fmt.Errorf("go-m4a: WriteFrame: %s frames vary in duration; use WriteFrameDuration", w.codec)
	}
	return w.WriteFrameDuration(au, w.defaultDuration)
}

// WriteFrameDuration appends one access unit to the mdat payload and records its
// size for the stsz table and sampleDuration (in the media timescale) for the stts
// table. sampleDuration is the number of samples per channel the access unit
// decodes to: 1024 for an AAC-LC AU, the packet's 48 kHz sample count for Opus, or
// the block size for FLAC. It rejects a nil or empty access unit, a zero duration,
// and any call after Close.
func (w *Writer) WriteFrameDuration(au []byte, sampleDuration uint32) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.closed {
		return ErrClosed
	}
	if len(au) == 0 {
		return fmt.Errorf("go-m4a: WriteFrameDuration: empty access unit")
	}
	if sampleDuration == 0 {
		return fmt.Errorf("go-m4a: WriteFrameDuration: sample duration must be positive")
	}
	if len(w.sizes) >= maxFrames {
		return fmt.Errorf("go-m4a: WriteFrameDuration: frame count would exceed the limit of %d", maxFrames)
	}
	if _, err := w.w.Write(au); err != nil {
		// A partial or failed write leaves the mdat payload out of sync with the
		// recorded sizes. Latch the error so no later WriteFrame or Close can
		// build a sample table that disagrees with the bytes on disk.
		w.writeErr = fmt.Errorf("go-m4a: write frame: %w", err)
		return w.writeErr
	}
	// Record the duration. While it stays uniform, keep only sampleDelta so no
	// per-frame slice is allocated (the common AAC and fixed-packet-Opus case).
	// Materialize durations, backfilling the uniform prefix, on the first divergence.
	switch {
	case w.durations != nil:
		w.durations = append(w.durations, sampleDuration)
	case len(w.sizes) == 0:
		w.sampleDelta = sampleDuration
	case sampleDuration != w.sampleDelta:
		w.durations = make([]uint32, len(w.sizes), len(w.sizes)+1)
		for i := range w.durations {
			w.durations[i] = w.sampleDelta
		}
		w.durations = append(w.durations, sampleDuration)
	}
	w.sizes = append(w.sizes, uint32(len(au)))
	w.totalPayload += int64(len(au))
	return nil
}

// SetSTREAMINFO supplies the 34-byte FLAC STREAMINFO metadata block that the dfLa
// box carries, for a CodecFLAC writer created with an empty WriterConfig.STREAMINFO.
// It exists because go-flac's StreamInfoBytes is only final after the whole encode
// (it records the measured min/max frame sizes and MD5), while the dfLa box is not
// built until Close: a caller can stream every frame first, then set STREAMINFO
// just before Close, avoiding buffering the clip. Calling it again overwrites the
// block, and calling it on a writer that was given STREAMINFO at NewWriter simply
// replaces that value.
//
// It copies the bytes, so a later mutation of streamInfo by the caller cannot
// change the box. It returns an error if the writer is not FLAC, if streamInfo is
// not exactly flacStreamInfoLen bytes, or if the writer is already closed
// (ErrClosed), the last so a block set after finalize cannot be silently ignored.
func (w *Writer) SetSTREAMINFO(streamInfo []byte) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.closed {
		return ErrClosed
	}
	if w.codec != CodecFLAC {
		return fmt.Errorf("go-m4a: SetSTREAMINFO: writer codec is %s, not FLAC", w.codec)
	}
	if len(streamInfo) != flacStreamInfoLen {
		return fmt.Errorf("go-m4a: SetSTREAMINFO: STREAMINFO is %d bytes, want %d", len(streamInfo), flacStreamInfoLen)
	}
	w.streamInfo = append([]byte(nil), streamInfo...)
	return nil
}

// Close finalizes the file: it patches the streamed mdat largesize, seeks past
// the payload, and writes the moov metadata (mvhd, trak with tkhd, optional
// edts/elst, and mdia down to the sample tables). It reports an error if no
// frames were written or a write fails. After a successful Close a second call
// returns ErrClosed. A Close that fails on a transient Seek or Write may be
// retried (WriteFrame stays rejected in between); a Close after a failed
// WriteFrame returns that latched error and writes nothing.
func (w *Writer) Close() error {
	if w.finalized {
		return ErrClosed // already finalized; a second Close is a no-op error
	}
	if w.writeErr != nil {
		return w.writeErr // a prior WriteFrame failed; the file is incomplete
	}
	// Mark closed so any further WriteFrame is rejected, even if a transient
	// finalize failure below leaves the file unfinished. finalized is set only
	// on success, so the caller may retry Close after a transient Seek/Write
	// error without WriteFrame being able to append in between.
	w.closed = true
	if len(w.sizes) == 0 {
		return fmt.Errorf("go-m4a: Close: no frames written")
	}
	// A FLAC track must carry a STREAMINFO block by finalize: it was either given
	// at NewWriter or supplied later with SetSTREAMINFO. Enforce the exact length
	// here rather than trusting the setter alone, so a writer created with an empty
	// STREAMINFO that never got SetSTREAMINFO fails with a clear error instead of
	// building a dfLa box around a zero-length block.
	if w.codec == CodecFLAC && len(w.streamInfo) != flacStreamInfoLen {
		return fmt.Errorf("go-m4a: Close: FLAC track has no STREAMINFO; call SetSTREAMINFO before Close")
	}

	// Overwrite the 8-byte mdat largesize in place: header size + payload.
	largesize := uint64(box.MdatHeaderSize) + uint64(w.totalPayload)
	if _, err := w.w.Seek(w.mdatBoxOffset+box.MdatLargeSizeOffset, io.SeekStart); err != nil {
		return fmt.Errorf("go-m4a: seek mdat largesize: %w", err)
	}
	if _, err := w.w.Write(box.AppendMdatLargeSize(nil, largesize)); err != nil {
		return fmt.Errorf("go-m4a: patch mdat largesize: %w", err)
	}

	// moov follows the mdat payload.
	moovOffset := w.payloadStart + w.totalPayload
	if _, err := w.w.Seek(moovOffset, io.SeekStart); err != nil {
		return fmt.Errorf("go-m4a: seek to moov: %w", err)
	}
	if _, err := w.w.Write(w.buildMoov()); err != nil {
		return fmt.Errorf("go-m4a: write moov: %w", err)
	}
	w.finalized = true
	return nil
}

// moovSpec is the per-call input to buildMoovFrom: everything that differs
// between a plain file's moov (Writer.buildMoov) and a fragmented stream's init
// segment (InitSegment). Everything not named here, the minf/mdia/trak skeleton
// and the "soun"/"SoundHandler" handler, is fixed and lives once in buildMoovFrom
// so a box added to one path cannot silently diverge from the other.
type moovSpec struct {
	// stbl is the caller-built sample table: populated for a plain file, the four
	// empty tables for an init segment. It is the one child buildMoovFrom does not
	// assemble itself, because its contents are exactly what the two paths differ on.
	stbl []byte

	// mediaDuration is the mdhd duration; presentationDuration is the tkhd and mvhd
	// duration. Both are zero for an init segment, whose timeline the fragments carry.
	mediaDuration        uint64
	presentationDuration uint64

	// editList requests an edts/elst with these segment_duration and media_time
	// values; when it is false, no edit list is emitted at all.
	editList        bool
	segmentDuration uint64
	mediaTime       int64

	// fragmented appends the mvex/trex that declares the movie fragmented, using
	// the track's default sample duration. Only the init segment sets it.
	fragmented bool

	// prefix is prepended before the moov box: the ftyp for an init segment, nil
	// for a plain file (whose ftyp and mdat are written separately).
	prefix []byte
}

// buildMoovFrom assembles the moov box shared by the plain and fragmented writers
// from spec, keeping the trak/mdia/minf skeleton and the sound-handler literals in
// one place. Both Writer.buildMoov and InitSegment call it, so the two outputs stay
// structurally identical by construction. The track_ID is fragmentTrackID (1) on
// both paths.
func (m *trackMeta) buildMoovFrom(spec moovSpec) []byte {
	// minf: sound media header, self-contained data reference, sample table.
	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, spec.stbl)

	// mdia: media header, sound handler, media information.
	var mdia []byte
	mdia = box.AppendMdhd(mdia, m.timescale, spec.mediaDuration)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	// trak: track header, optional edit list, media. Order is tkhd, edts, mdia.
	var trak []byte
	trak = box.AppendTkhd(trak, fragmentTrackID, spec.presentationDuration)
	if spec.editList {
		trak = box.AppendEdts(trak, box.AppendElst(nil, spec.segmentDuration, spec.mediaTime))
	}
	trak = box.AppendMdia(trak, mdia)

	// moov: movie header, the single track, then the fragment declaration if any.
	moov := box.AppendMvhd(nil, m.timescale, spec.presentationDuration)
	moov = box.AppendTrak(moov, trak)
	if spec.fragmented {
		// mvex/trex declares the movie fragmented. default_sample_duration is the
		// codec's fixed frame length where it has one (AAC-LC's 1024) and zero
		// otherwise, in which case each fragment states its own.
		moov = box.AppendMvex(moov, box.AppendTrex(nil,
			fragmentTrackID, m.defaultDuration, box.SyncSampleFlags))
	}
	return box.AppendMoov(spec.prefix, moov)
}

// buildMoov assembles the complete moov box from the accumulated sample sizes,
// the per-sample durations, and the resolved edit-list parameters. Movie and media
// timescales both equal w.timescale (the sample rate, which is 48000 for Opus), so
// the elst media_time and segment_duration share one unit.
func (w *Writer) buildMoov() []byte {
	frameCount := uint32(len(w.sizes))
	mediaDuration := w.mediaDuration()

	editList := w.encoderDelay != NoEdit
	presentationDuration := mediaDuration
	var segmentDuration uint64
	var mediaTime int64
	if editList {
		delay := w.resolveDelay(w.encoderDelay)
		mediaTime = int64(delay)
		if w.mediaLength > 0 {
			segmentDuration = uint64(w.mediaLength)
		} else {
			// Post-priming samples. Clamp at zero so a delay larger than the
			// media (a misconfiguration) never wraps the unsigned field.
			presented := int64(mediaDuration) - int64(delay)
			if presented < 0 {
				presented = 0
			}
			segmentDuration = uint64(presented)
		}
		presentationDuration = segmentDuration
	}

	// stbl: sample description, timing, and location tables. The single chunk
	// starts at payloadStart, which is well within uint32, so stco always fits.
	// Pre-size the buffer to the known final length so the stsz entry list
	// (4 bytes per sample, the dominant term) does not trigger a chain of
	// growslice reallocations. The len(w.streamInfo)+256 term generously covers the
	// fixed codec sample entry (mp4a/esds, Opus/dOps, or fLaC/dfLa) and the
	// stts/stsc/stco overhead. Computed in int64 and clamped to maxInt so the
	// 4*len(w.sizes) term cannot overflow a 32-bit int at the frame-count limit; this
	// only sets capacity, so the appended bytes and the output file are unchanged.
	capHint := 4*int64(len(w.sizes)) + int64(len(w.streamInfo)) + 256
	if capHint > maxInt {
		capHint = maxInt
	}
	stbl := make([]byte, 0, int(capHint))
	stbl = box.AppendStsdEntry(stbl, w.sampleEntry())
	stbl = w.appendStts(stbl)
	stbl = box.AppendStsc(stbl, 1, frameCount, 1)
	stbl = box.AppendStsz(stbl, w.sizes)
	stbl = box.AppendStco(stbl, []uint32{uint32(w.payloadStart)})

	return w.buildMoovFrom(moovSpec{
		stbl:                 stbl,
		mediaDuration:        mediaDuration,
		presentationDuration: presentationDuration,
		editList:             editList,
		segmentDuration:      segmentDuration,
		mediaTime:            mediaTime,
	})
}

// mediaDuration is the total decode duration of every access unit, in the media
// timescale. When durations are uniform (durations nil) it is frameCount*sampleDelta
// in O(1); otherwise it sums the per-frame durations.
func (w *Writer) mediaDuration() uint64 {
	if w.durations == nil {
		return uint64(len(w.sizes)) * uint64(w.sampleDelta)
	}
	var d uint64
	for _, x := range w.durations {
		d += uint64(x)
	}
	return d
}

// appendStts appends the stts table. Uniform durations (AAC-LC's 1024, Opus's
// fixed packet length) collapse to one (frameCount, sampleDelta) run, byte-identical
// to the pre-generalization writer; a diverging codec (FLAC's short final frame)
// emits a run-length-encoded multi-run table from the per-frame durations.
func (w *Writer) appendStts(dst []byte) []byte {
	if w.durations == nil {
		return box.AppendStts(dst, uint32(len(w.sizes)), w.sampleDelta)
	}
	var runs []box.SttsRun
	for _, d := range w.durations {
		if n := len(runs); n > 0 && runs[n-1].Delta == d {
			runs[n-1].Count++
			continue
		}
		runs = append(runs, box.SttsRun{Count: 1, Delta: d})
	}
	return box.AppendSttsRuns(dst, runs)
}

// sampleEntry builds the codec-specific stsd sample entry: mp4a+esds (AAC-LC),
// Opus+dOps, or fLaC+dfLa. The fragmented init segment reuses it verbatim, so both
// paths describe the track identically.
func (m *trackMeta) sampleEntry() []byte {
	switch m.codec {
	case CodecOpus:
		dops := box.AppendDops(nil, uint8(m.channels), m.opusPreSkip, m.opusInputRate)
		return box.AppendOpusEntry(nil, m.channels, m.sampleRate, dops)
	case CodecFLAC:
		dfla := box.AppendDfla(nil, m.streamInfo)
		return box.AppendFlacEntry(nil, m.channels, m.sampleRate, dfla)
	default:
		return box.AppendMp4a(nil, m.channels, m.sampleRate, m.asc)
	}
}

// resolveDelay returns the priming trim in media-timescale samples for an
// EncoderDelay field: zero selects the codec's default, a positive value is taken
// literally, and the NoEdit sentinel resolves to zero so a caller that has not
// already intercepted it cannot turn -1 into a negative edit-list media_time.
func (m *trackMeta) resolveDelay(encoderDelay int) int {
	switch encoderDelay {
	case NoEdit:
		return 0
	case 0:
		return m.defaultDelay
	default:
		return encoderDelay
	}
}

// validateConfig checks every field of cfg and cross-checks SampleRate and
// Channels against the AudioSpecificConfig. All messages are prefixed
// "go-m4a: " to match the package error convention.
func validateConfig(cfg WriterConfig) error {
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return fmt.Errorf("go-m4a: channels %d out of range, want 1 or 2", cfg.Channels)
	}
	if cfg.MediaLength < 0 {
		return fmt.Errorf("go-m4a: media length %d is negative", cfg.MediaLength)
	}
	if cfg.EncoderDelay < NoEdit {
		return fmt.Errorf("go-m4a: encoder delay %d is invalid, want >= %d", cfg.EncoderDelay, NoEdit)
	}
	if cfg.Brand != "" && len(cfg.Brand) != 4 {
		return fmt.Errorf("go-m4a: brand %q must be exactly 4 bytes", cfg.Brand)
	}
	if cfg.SampleRate <= 0 {
		return fmt.Errorf("go-m4a: sample rate %d Hz is not positive", cfg.SampleRate)
	}
	// The 16.16 sample-entry ceiling is no longer checked here: it is not shared by
	// every codec. AAC rejects a rate above it, FLAC accepts higher rates and writes
	// a reduced sample-entry hint, and Opus is pinned below it. Each per-codec
	// validator applies the rule that fits its rate model.

	switch cfg.Codec {
	case CodecAACLC:
		return validateAACConfig(cfg)
	case CodecOpus:
		return validateOpusConfig(cfg)
	case CodecFLAC:
		return validateFLACConfig(cfg)
	default:
		return fmt.Errorf("go-m4a: unknown codec %d", cfg.Codec)
	}
}

// validateAACConfig cross-checks the AAC-LC AudioSpecificConfig against SampleRate
// and Channels.
func validateAACConfig(cfg WriterConfig) error {
	if len(cfg.ASC) < 2 {
		return fmt.Errorf("go-m4a: ASC too short: %d bytes, need at least 2", len(cfg.ASC))
	}
	if cfg.SampleRate > maxAudioSampleEntryRate {
		// 88200 and 96000 are AAC-LC sampling-frequency-table rates, but they do not
		// fit the 16.16 sample-entry samplerate field, and AAC carries no
		// authoritative rate elsewhere (unlike FLAC's STREAMINFO), so writing the
		// reduced fallback would silently misreport the track. Reject rather than
		// corrupt. This is checked before the table lookup so the message names the
		// real problem.
		return fmt.Errorf("go-m4a: sample rate %d Hz exceeds the samplerate field maximum of %d Hz", cfg.SampleRate, maxAudioSampleEntryRate)
	}
	// This guard cannot change which configs are accepted: an off-table rate can
	// never match the ASC either, because the rate the ASC agrees with is read out
	// of this same table, so the cross-check below would reject it regardless.
	// What it changes is which error the caller gets, and that is worth the three
	// lines: "unsupported sample rate 47999 Hz" says the rate is not one AAC can
	// express, where "does not match ASC (48000 Hz)" would send the caller looking
	// for a mismatch they cannot fix by editing the ASC.
	if !aacRateSupported(cfg.SampleRate) {
		return fmt.Errorf("go-m4a: unsupported sample rate %d Hz", cfg.SampleRate)
	}
	aot, sfi, chanConfig := parseASC(cfg.ASC)
	if aot != audioObjectTypeAACLC {
		return fmt.Errorf("go-m4a: ASC audio object type %d is not AAC-LC (%d)", aot, audioObjectTypeAACLC)
	}
	if int(sfi) >= len(samplingFrequencyTable) {
		return fmt.Errorf("go-m4a: ASC sampling frequency index %d is unsupported", sfi)
	}
	if ascRate := samplingFrequencyTable[sfi]; ascRate != cfg.SampleRate {
		return fmt.Errorf("go-m4a: sample rate %d Hz does not match ASC (%d Hz)", cfg.SampleRate, ascRate)
	}
	if int(chanConfig) != cfg.Channels {
		return fmt.Errorf("go-m4a: channels %d does not match ASC channel configuration %d", cfg.Channels, chanConfig)
	}
	return nil
}

// validateOpusConfig checks the Opus-specific fields. The Opus encapsulation fixes
// the container timescale at 48000, so SampleRate must be 48000.
func validateOpusConfig(cfg WriterConfig) error {
	if cfg.SampleRate != opusTimescale {
		return fmt.Errorf("go-m4a: Opus requires SampleRate %d, got %d", opusTimescale, cfg.SampleRate)
	}
	// PreSkip is a 16-bit dOps field and InputSampleRate a 32-bit one; reject values
	// that would silently wrap on the narrowing conversion in NewWriter rather than
	// write a wrong dOps box.
	if cfg.OpusPreSkip < 0 || cfg.OpusPreSkip > math.MaxUint16 {
		return fmt.Errorf("go-m4a: Opus pre-skip %d out of range, want 0..%d", cfg.OpusPreSkip, math.MaxUint16)
	}
	if cfg.OpusInputSampleRate < 0 || int64(cfg.OpusInputSampleRate) > math.MaxUint32 {
		return fmt.Errorf("go-m4a: Opus input sample rate %d out of range, want 0..%d", cfg.OpusInputSampleRate, uint32(math.MaxUint32))
	}
	return nil
}

// validateFLACConfig checks the STREAMINFO metadata block. It accepts either the
// full 34 bytes supplied up front, or an empty slice, which defers the block to a
// later Writer.SetSTREAMINFO call. Deferral exists because go-flac's
// StreamInfoBytes is only final after the whole encode (it carries the measured
// min/max frame sizes and MD5), while the dfLa box is not built until Close; the
// flacm4a bridge relies on it to stream frames without buffering the clip. Any
// other length is rejected. Close enforces that a non-empty block is present by
// the time the file is finalized (flacStreamInfoLen bytes), so an empty config
// that never gets SetSTREAMINFO fails there rather than writing a malformed dfLa.
func validateFLACConfig(cfg WriterConfig) error {
	if cfg.SampleRate > maxFLACSampleRate {
		// FLAC's STREAMINFO sample-rate field is 20 bits, so the format cannot even
		// declare a higher rate. Reject rather than write a truncated STREAMINFO.
		return fmt.Errorf("go-m4a: FLAC sample rate %d Hz exceeds the maximum of %d Hz", cfg.SampleRate, maxFLACSampleRate)
	}
	if len(cfg.STREAMINFO) != 0 && len(cfg.STREAMINFO) != flacStreamInfoLen {
		return fmt.Errorf("go-m4a: FLAC STREAMINFO is %d bytes, want %d or 0 (set later via SetSTREAMINFO)", len(cfg.STREAMINFO), flacStreamInfoLen)
	}
	return nil
}

// aacRateSupported reports whether rate appears in the AAC sampling frequency
// table, i.e. whether it is a rate the writer can encode into an ASC index. It
// is AAC-specific, as the name says: Opus and FLAC do not consult this table,
// so it is not the package's answer to "is this rate supported".
func aacRateSupported(rate int) bool {
	for _, r := range samplingFrequencyTable {
		if r == rate {
			return true
		}
	}
	return false
}

// parseASC extracts the three leading AAC-LC AudioSpecificConfig fields from the
// first two bytes: audioObjectType (5 bits), samplingFrequencyIndex (4 bits),
// and channelConfiguration (4 bits). The caller has already ensured asc has at
// least two bytes. Extended forms (audioObjectType 31, samplingFrequencyIndex
// 15) are out of v1 scope; the returned index simply fails the table lookup.
func parseASC(asc []byte) (audioObjectType, samplingFrequencyIndex, channelConfiguration uint8) {
	b0, b1 := asc[0], asc[1]
	audioObjectType = b0 >> 3
	samplingFrequencyIndex = ((b0 & 0x07) << 1) | (b1 >> 7)
	channelConfiguration = (b1 >> 3) & 0x0f
	return audioObjectType, samplingFrequencyIndex, channelConfiguration
}
