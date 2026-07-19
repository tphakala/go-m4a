// SPDX-License-Identifier: MIT

// Package m4a muxes AAC-LC access units into an MP4/M4A container and demuxes
// them back out. It is the container half that go-aac deliberately leaves to an
// external muxer: an edit list (elst) trims the encoder priming so the written
// file is sample-accurate and gapless. The public surface is stdlib-only; the
// ISO-BMFF byte mechanics live in the internal/box package, whose layout is
// fixed by docs/box-layout.md.
package m4a

import (
	"fmt"
	"io"

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

// maxAudioSampleEntryRate is the largest sample rate the mp4a AudioSampleEntry
// samplerate field can represent. It is a 16.16 fixed-point value, so the
// integer rate must fit 16 bits; a higher rate would silently wrap when shifted
// into the field. The supported 44100 and 48000 Hz (and 64000) are well within
// it; 88200 and 96000 are not, so they are rejected rather than written wrong.
const maxAudioSampleEntryRate = 0xFFFF

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

// WriterConfig configures a Writer. SampleRate and Channels must agree with ASC;
// NewWriter validates them against it and refuses a mismatch.
type WriterConfig struct {
	// Codec selects the audio codec. The zero value is CodecAACLC, so a config
	// that sets only ASC keeps muxing AAC-LC. For CodecOpus set OpusPreSkip and
	// OpusInputSampleRate (and SampleRate must be 48000); for CodecFLAC set
	// STREAMINFO.
	Codec Codec

	// SampleRate is the audio sample rate in Hz (for example 48000). Required. For
	// AAC-LC it must match the rate encoded in ASC; for Opus it must be 48000 (the
	// fixed Opus container timescale).
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
	// dfLa box (from go-flac's pcm.FrameEncoder.StreamInfoBytes). Required for
	// CodecFLAC; ignored otherwise.
	STREAMINFO []byte

	// EncoderDelay is the number of leading priming samples to trim with an edit
	// list. Zero uses DefaultEncoderDelay (1024); NoEdit writes no edit list at
	// all; a positive value trims exactly that many samples.
	EncoderDelay int

	// MediaLength, when greater than zero, is the number of PCM samples per
	// channel the source contained. It sets the edit-list segment duration
	// exactly, so trailing final-frame padding is also excluded. Zero presents
	// every decoded sample after the priming.
	MediaLength int64

	// Brand overrides the ftyp major brand (default "M4A "). When set it must be
	// exactly four bytes (space-padded, for example "mp42"); NewWriter rejects
	// any other length. The compatible brands always include "M4A ", "mp42", and
	// "isom".
	Brand string
}

// Writer streams AAC-LC access units into an MP4/M4A file. The on-disk layout is
// ftyp | mdat | moov: ftyp and the mdat header are written up front, each
// WriteFrame appends one access unit to the mdat payload, and Close patches the
// mdat size and writes the moov metadata. It requires an io.WriteSeeker because
// the mdat size is a placeholder patched once at Close.
type Writer struct {
	w io.WriteSeeker

	// Normalized configuration, captured at NewWriter so a later mutation of the
	// caller's WriterConfig or byte slices cannot change the output.
	codec         Codec
	sampleRate    uint32
	channels      uint16
	asc           []byte // AAC-LC AudioSpecificConfig
	streamInfo    []byte // FLAC STREAMINFO (dfLa payload)
	opusPreSkip   uint16
	opusInputRate uint32
	encoderDelay  int
	mediaLength   int64

	// timescale is the media and movie timescale: SampleRate for AAC and FLAC,
	// 48000 for Opus. defaultDelay is the codec's priming trim when EncoderDelay is
	// left at zero. defaultDuration is the per-sample duration WriteFrame records
	// for a fixed-duration codec (1024 for AAC-LC); it is zero for Opus and FLAC,
	// whose callers must use WriteFrameDuration.
	timescale       uint32
	defaultDelay    int
	defaultDuration uint32

	// durations holds each access unit's decode duration in the media timescale,
	// in write order, for the stts table.
	durations []uint32

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

// NewWriter validates cfg against its ASC, then writes the ftyp box and the
// placeholder mdat header to w. It returns an error, prefixed "go-m4a: ", when
// the writer is nil, the ASC is malformed or disagrees with SampleRate or
// Channels, the sample rate is unsupported, or an initial write fails.
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

	wr := &Writer{
		w:             w,
		codec:         cfg.Codec,
		sampleRate:    uint32(cfg.SampleRate),
		channels:      uint16(cfg.Channels),
		encoderDelay:  cfg.EncoderDelay,
		mediaLength:   cfg.MediaLength,
		timescale:     uint32(cfg.SampleRate),
		mdatBoxOffset: int64(len(ftyp)),
		payloadStart:  int64(len(ftyp) + box.MdatHeaderSize),
	}
	switch cfg.Codec {
	case CodecAACLC:
		wr.asc = append([]byte(nil), cfg.ASC...)
		wr.defaultDelay = DefaultEncoderDelay // 1024
		wr.defaultDuration = samplesPerFrame  // every AAC-LC AU is 1024 samples
	case CodecOpus:
		// The Opus timescale is fixed at 48000; validateConfig enforced SampleRate.
		wr.opusPreSkip = uint16(cfg.OpusPreSkip)
		if wr.opusPreSkip == 0 {
			wr.opusPreSkip = DefaultOpusPreSkip
		}
		wr.opusInputRate = uint32(cfg.OpusInputSampleRate)
		if wr.opusInputRate == 0 {
			wr.opusInputRate = uint32(cfg.SampleRate)
		}
		wr.defaultDelay = int(wr.opusPreSkip)
		// Opus packet durations vary (the final packet may be short), so callers
		// supply each with WriteFrameDuration; defaultDuration stays 0.
	case CodecFLAC:
		wr.streamInfo = append([]byte(nil), cfg.STREAMINFO...)
		wr.defaultDelay = 0 // FLAC has no encoder priming
		// FLAC block sizes vary (the final frame is short); callers supply each
		// with WriteFrameDuration; defaultDuration stays 0.
	}
	return wr, nil
}

// WriteFrame appends one access unit to the mdat payload using the codec's fixed
// per-sample duration. It works for AAC-LC, whose every AU is 1024 samples. For
// Opus and FLAC, whose frames vary in duration, it returns an error directing the
// caller to WriteFrameDuration. It rejects a nil or empty access unit, and any
// call after Close.
func (w *Writer) WriteFrame(au []byte) error {
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
		return fmt.Errorf("go-m4a: WriteFrame: empty access unit")
	}
	if sampleDuration == 0 {
		return fmt.Errorf("go-m4a: WriteFrame: zero sample duration; codec %s requires an explicit duration via WriteFrameDuration", w.codec)
	}
	if len(w.sizes) >= maxFrames {
		return fmt.Errorf("go-m4a: WriteFrame: frame count would exceed the limit of %d", maxFrames)
	}
	if _, err := w.w.Write(au); err != nil {
		// A partial or failed write leaves the mdat payload out of sync with the
		// recorded sizes. Latch the error so no later WriteFrame or Close can
		// build a sample table that disagrees with the bytes on disk.
		w.writeErr = fmt.Errorf("go-m4a: write frame: %w", err)
		return w.writeErr
	}
	w.sizes = append(w.sizes, uint32(len(au)))
	w.durations = append(w.durations, sampleDuration)
	w.totalPayload += int64(len(au))
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

// buildMoov assembles the complete moov box from the accumulated sample sizes,
// the per-sample durations, and the resolved edit-list parameters. Movie and media
// timescales both equal w.timescale (the sample rate, which is 48000 for Opus), so
// the elst media_time and segment_duration share one unit.
func (w *Writer) buildMoov() []byte {
	frameCount := uint32(len(w.sizes))
	mediaDuration := w.totalDuration()

	editList := w.encoderDelay != NoEdit
	presentationDuration := mediaDuration
	var segmentDuration uint64
	var mediaTime int64
	if editList {
		delay := w.encoderDelay
		if delay == 0 {
			delay = w.defaultDelay
		}
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
	// growslice reallocations. The len(w.asc)+256 term generously covers the
	// fixed stsd/mp4a/esds/stts/stsc/stco overhead. This only sets capacity; the
	// appended bytes, and thus the output file, are unchanged.
	stbl := make([]byte, 0, 4*len(w.sizes)+len(w.streamInfo)+256)
	stbl = box.AppendStsdEntry(stbl, w.sampleEntry())
	stbl = box.AppendSttsRuns(stbl, w.sttsRuns())
	stbl = box.AppendStsc(stbl, 1, frameCount, 1)
	stbl = box.AppendStsz(stbl, w.sizes)
	stbl = box.AppendStco(stbl, []uint32{uint32(w.payloadStart)})

	// minf: sound media header, self-contained data reference, sample table.
	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, stbl)

	// mdia: media header, sound handler, media information.
	var mdia []byte
	mdia = box.AppendMdhd(mdia, w.timescale, mediaDuration)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	// trak: track header, optional edit list, media. Order is tkhd, edts, mdia.
	var trak []byte
	trak = box.AppendTkhd(trak, 1, presentationDuration)
	if editList {
		trak = box.AppendEdts(trak, box.AppendElst(nil, segmentDuration, mediaTime))
	}
	trak = box.AppendMdia(trak, mdia)

	// moov: movie header then the single track.
	moov := box.AppendMvhd(nil, w.timescale, presentationDuration)
	moov = box.AppendTrak(moov, trak)
	return box.AppendMoov(nil, moov)
}

// totalDuration is the sum of every access unit's decode duration, in the media
// timescale.
func (w *Writer) totalDuration() uint64 {
	var d uint64
	for _, x := range w.durations {
		d += uint64(x)
	}
	return d
}

// sttsRuns run-length-encodes the per-sample durations into stts entries. AAC-LC
// collapses to a single (frameCount, 1024) run; Opus and FLAC produce a run of the
// nominal duration plus a short final run.
func (w *Writer) sttsRuns() []box.SttsRun {
	var runs []box.SttsRun
	for _, d := range w.durations {
		if n := len(runs); n > 0 && runs[n-1].Delta == d {
			runs[n-1].Count++
			continue
		}
		runs = append(runs, box.SttsRun{Count: 1, Delta: d})
	}
	return runs
}

// sampleEntry builds the codec-specific stsd sample entry: mp4a+esds (AAC-LC),
// Opus+dOps, or fLaC+dfLa.
func (w *Writer) sampleEntry() []byte {
	switch w.codec {
	case CodecOpus:
		dops := box.AppendDops(nil, uint8(w.channels), w.opusPreSkip, w.opusInputRate)
		return box.AppendOpusEntry(nil, w.channels, w.sampleRate, dops)
	case CodecFLAC:
		dfla := box.AppendDfla(nil, w.streamInfo)
		return box.AppendFlacEntry(nil, w.channels, w.sampleRate, dfla)
	default:
		return box.AppendMp4a(nil, w.channels, w.sampleRate, w.asc)
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
	if cfg.SampleRate > maxAudioSampleEntryRate {
		// The AudioSampleEntry samplerate is a 16.16 field, so a rate above 65535 Hz
		// cannot be represented and would be written wrong. 88200 and 96000 Hz are
		// the affected AAC rates; reject rather than corrupt.
		return fmt.Errorf("go-m4a: sample rate %d Hz exceeds the samplerate field maximum of %d Hz", cfg.SampleRate, maxAudioSampleEntryRate)
	}

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
	if !rateSupported(cfg.SampleRate) {
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
	if cfg.OpusPreSkip < 0 {
		return fmt.Errorf("go-m4a: Opus pre-skip %d is negative", cfg.OpusPreSkip)
	}
	if cfg.OpusInputSampleRate < 0 {
		return fmt.Errorf("go-m4a: Opus input sample rate %d is negative", cfg.OpusInputSampleRate)
	}
	return nil
}

// validateFLACConfig checks that the STREAMINFO metadata block is present and the
// expected 34 bytes.
func validateFLACConfig(cfg WriterConfig) error {
	const streamInfoLen = 34
	if len(cfg.STREAMINFO) != streamInfoLen {
		return fmt.Errorf("go-m4a: FLAC STREAMINFO is %d bytes, want %d", len(cfg.STREAMINFO), streamInfoLen)
	}
	return nil
}

// rateSupported reports whether rate appears in the AAC sampling frequency
// table, i.e. whether it is a rate the writer can encode into an ASC index.
func rateSupported(rate int) bool {
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
