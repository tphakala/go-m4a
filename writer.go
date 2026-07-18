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
	// SampleRate is the audio sample rate in Hz (for example 48000). Required,
	// and it must match the rate encoded in ASC.
	SampleRate int

	// Channels is the channel count, 1 (mono) or 2 (stereo). Required, and it
	// must match the channel configuration encoded in ASC.
	Channels int

	// ASC is the MPEG-4 AudioSpecificConfig (two bytes for AAC-LC). Required.
	// The writer copies the bytes verbatim into the esds DecoderSpecificInfo.
	ASC []byte

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
	// caller's WriterConfig or ASC slice cannot change the output.
	sampleRate   uint32
	channels     uint16
	asc          []byte
	encoderDelay int
	mediaLength  int64

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

	return &Writer{
		w:             w,
		sampleRate:    uint32(cfg.SampleRate),
		channels:      uint16(cfg.Channels),
		asc:           append([]byte(nil), cfg.ASC...),
		encoderDelay:  cfg.EncoderDelay,
		mediaLength:   cfg.MediaLength,
		mdatBoxOffset: int64(len(ftyp)),
		payloadStart:  int64(len(ftyp) + box.MdatHeaderSize),
	}, nil
}

// WriteFrame appends one raw AAC-LC access unit to the mdat payload and records
// its size for the stsz table. It rejects a nil or empty access unit, and any
// call after Close, with an error.
func (w *Writer) WriteFrame(au []byte) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.closed {
		return ErrClosed
	}
	if len(au) == 0 {
		return fmt.Errorf("go-m4a: WriteFrame: empty access unit")
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
	w.totalPayload += int64(len(au))
	return nil
}

// Close finalizes the file: it patches the streamed mdat largesize, seeks past
// the payload, and writes the moov metadata (mvhd, trak with tkhd, optional
// edts/elst, and mdia down to the sample tables). It reports an error if no
// frames were written or a write fails. Close is not repeatable: a second call
// returns ErrClosed.
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

// buildMoov assembles the complete moov box from the accumulated sample sizes
// and the resolved edit-list parameters. Movie and media timescales both equal
// the sample rate, so the elst media_time and segment_duration share one unit.
func (w *Writer) buildMoov() []byte {
	frameCount := uint32(len(w.sizes))
	mediaDuration := uint64(frameCount) * samplesPerFrame

	editList := w.encoderDelay != NoEdit
	presentationDuration := mediaDuration
	var segmentDuration uint64
	var mediaTime int64
	if editList {
		delay := w.encoderDelay
		if delay == 0 {
			delay = DefaultEncoderDelay
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
	var stbl []byte
	stbl = box.AppendStsd(stbl, w.channels, w.sampleRate, w.asc)
	stbl = box.AppendStts(stbl, frameCount, samplesPerFrame)
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
	mdia = box.AppendMdhd(mdia, w.sampleRate, mediaDuration)
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
	moov := box.AppendMvhd(nil, w.sampleRate, presentationDuration)
	moov = box.AppendTrak(moov, trak)
	return box.AppendMoov(nil, moov)
}

// validateConfig checks every field of cfg and cross-checks SampleRate and
// Channels against the AudioSpecificConfig. All messages are prefixed
// "go-m4a: " to match the package error convention.
func validateConfig(cfg WriterConfig) error {
	if len(cfg.ASC) < 2 {
		return fmt.Errorf("go-m4a: ASC too short: %d bytes, need at least 2", len(cfg.ASC))
	}
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
	if !rateSupported(cfg.SampleRate) {
		return fmt.Errorf("go-m4a: unsupported sample rate %d Hz", cfg.SampleRate)
	}
	if cfg.SampleRate > maxAudioSampleEntryRate {
		// The mp4a AudioSampleEntry samplerate is a 16.16 field, so a rate above
		// 65535 Hz cannot be represented and would be written wrong. 88200 and
		// 96000 Hz are the affected AAC rates; reject rather than corrupt.
		return fmt.Errorf("go-m4a: sample rate %d Hz exceeds the mp4a samplerate field maximum of %d Hz", cfg.SampleRate, maxAudioSampleEntryRate)
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
