// SPDX-License-Identifier: MIT

package box

import (
	"encoding/binary"
	"fmt"
)

// Additional tfhd flag bits (ISO/IEC 14496-12 8.8.7) a reader must decode. The
// writer emits only tfhdDefaultBaseIsMoof and tfhdDefaultSampleDurationPresent
// (see AppendTfhd), but a foreign muxer may set any of these, and each gates an
// optional field in the box body. tfhdDefaultBaseIsMoof (0x020000) and
// tfhdDefaultSampleDurationPresent (0x000008) are declared in fragment.go.
const (
	tfhdBaseDataOffsetPresent         = 0x000001
	tfhdSampleDescriptionIndexPresent = 0x000002
	tfhdDefaultSampleSizePresent      = 0x000010
	tfhdDefaultSampleFlagsPresent     = 0x000020
	tfhdDurationIsEmpty               = 0x010000
)

// Additional trun flag bits (ISO/IEC 14496-12 8.8.8) a reader must decode.
// trunDataOffsetPresent (0x000001), trunSampleDurationPresent (0x000100), and
// trunSampleSizePresent (0x000200) are declared in fragment.go.
const (
	trunFirstSampleFlagsPresent             = 0x000004
	trunSampleFlagsPresent                  = 0x000400
	trunSampleCompositionTimeOffsetsPresent = 0x000800
)

// Tfhd holds the decoded track fragment header (tfhd) fields the demuxer needs.
// Each optional field carries a Has* companion because 0 is a legal value that
// must be distinguished from an absent field: an absent default falls through to
// the trex default, a present zero does not.
type Tfhd struct {
	TrackID                  uint32
	HasBaseDataOffset        bool
	BaseDataOffset           uint64
	DefaultBaseIsMoof        bool
	HasDefaultSampleDuration bool
	DefaultSampleDuration    uint32
	HasDefaultSampleSize     bool
	DefaultSampleSize        uint32
	HasDefaultSampleFlags    bool
	DefaultSampleFlags       uint32
	DurationIsEmpty          bool
}

// ParseTfhd decodes a tfhd (track fragment header) FullBox body. It reads the
// mandatory track_ID and then the flag-gated optional fields in their ISO order:
// base_data_offset(8, 0x000001), sample_description_index(4, 0x000002),
// default_sample_duration(4, 0x000008), default_sample_size(4, 0x000010),
// default_sample_flags(4, 0x000020). sample_description_index is skipped: a
// single-track fragmented file has one sample entry. Every optional field is
// bounds-checked against the remaining body before it is read.
func ParseTfhd(payload []byte) (Tfhd, error) {
	if len(payload) < 8 {
		return Tfhd{}, fmt.Errorf("tfhd: %d bytes, need 8: %w", len(payload), errParse)
	}
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	var t Tfhd
	t.TrackID = binary.BigEndian.Uint32(payload[4:])
	t.DefaultBaseIsMoof = flags&tfhdDefaultBaseIsMoof != 0
	t.DurationIsEmpty = flags&tfhdDurationIsEmpty != 0

	p := 8
	need := func(n int) error {
		if p+n > len(payload) {
			return fmt.Errorf("tfhd optional fields overrun %d bytes: %w", len(payload), errParse)
		}
		return nil
	}
	if flags&tfhdBaseDataOffsetPresent != 0 {
		if err := need(8); err != nil {
			return Tfhd{}, err
		}
		t.HasBaseDataOffset = true
		t.BaseDataOffset = binary.BigEndian.Uint64(payload[p:])
		p += 8
	}
	if flags&tfhdSampleDescriptionIndexPresent != 0 {
		if err := need(4); err != nil {
			return Tfhd{}, err
		}
		p += 4 // sample_description_index: single stsd entry, not needed
	}
	if flags&tfhdDefaultSampleDurationPresent != 0 {
		if err := need(4); err != nil {
			return Tfhd{}, err
		}
		t.HasDefaultSampleDuration = true
		t.DefaultSampleDuration = binary.BigEndian.Uint32(payload[p:])
		p += 4
	}
	if flags&tfhdDefaultSampleSizePresent != 0 {
		if err := need(4); err != nil {
			return Tfhd{}, err
		}
		t.HasDefaultSampleSize = true
		t.DefaultSampleSize = binary.BigEndian.Uint32(payload[p:])
		p += 4
	}
	if flags&tfhdDefaultSampleFlagsPresent != 0 {
		if err := need(4); err != nil {
			return Tfhd{}, err
		}
		t.HasDefaultSampleFlags = true
		t.DefaultSampleFlags = binary.BigEndian.Uint32(payload[p:])
		p += 4
	}
	return t, nil
}

// ParseTfdt decodes the baseMediaDecodeTime from a tfdt (track fragment decode
// time) FullBox body, honoring both the version-0 (32-bit) and version-1 (64-bit)
// widths. The writer emits only version 1 (see AppendTfdt); a foreign muxer may
// emit version 0.
func ParseTfdt(payload []byte) (baseMediaDecodeTime uint64, err error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("tfdt: %d bytes: %w", len(payload), errParse)
	}
	switch payload[0] {
	case 1:
		if len(payload) < 12 {
			return 0, fmt.Errorf("tfdt v1: %d bytes, need 12: %w", len(payload), errParse)
		}
		return binary.BigEndian.Uint64(payload[4:]), nil
	case 0:
		if len(payload) < 8 {
			return 0, fmt.Errorf("tfdt v0: %d bytes, need 8: %w", len(payload), errParse)
		}
		return uint64(binary.BigEndian.Uint32(payload[4:])), nil
	default:
		// A reserved version defines a layout this parser does not know; reading it
		// as v0 would decode the wrong field, so reject it rather than guess.
		return 0, fmt.Errorf("tfdt version %d unsupported: %w", payload[0], errParse)
	}
}

// Trex holds the decoded track extends (trex) defaults, which a movie fragment
// inherits when its tfhd or trun does not override them.
type Trex struct {
	TrackID                       uint32
	DefaultSampleDescriptionIndex uint32
	DefaultSampleDuration         uint32
	DefaultSampleSize             uint32
	DefaultSampleFlags            uint32
}

// ParseTrex decodes a trex (track extends) FullBox body: a fixed layout of
// version/flags(4) + track_ID(4) + default_sample_description_index(4) +
// default_sample_duration(4) + default_sample_size(4) + default_sample_flags(4).
func ParseTrex(payload []byte) (Trex, error) {
	const need = 24
	if len(payload) < need {
		return Trex{}, fmt.Errorf("trex: %d bytes, need %d: %w", len(payload), need, errParse)
	}
	return Trex{
		TrackID:                       binary.BigEndian.Uint32(payload[4:]),
		DefaultSampleDescriptionIndex: binary.BigEndian.Uint32(payload[8:]),
		DefaultSampleDuration:         binary.BigEndian.Uint32(payload[12:]),
		DefaultSampleSize:             binary.BigEndian.Uint32(payload[16:]),
		DefaultSampleFlags:            binary.BigEndian.Uint32(payload[20:]),
	}, nil
}

// ParseMfhd decodes the sequence_number from an mfhd (movie fragment header)
// FullBox body. The demuxer does not require it for AU extraction; it is exposed
// for symmetry with AppendMfhd and an optional monotonicity check.
func ParseMfhd(payload []byte) (sequenceNumber uint32, err error) {
	if len(payload) < 8 {
		return 0, fmt.Errorf("mfhd: %d bytes, need 8: %w", len(payload), errParse)
	}
	return binary.BigEndian.Uint32(payload[4:]), nil
}

// ParseTkhd decodes the track_ID from a tkhd (track header) FullBox body,
// honoring the version-0 (32-bit time fields) and version-1 (64-bit time fields)
// layouts. The demuxer uses it to bind a track's movie fragments to its sample
// entry: a moof's traf carries the track_ID, not the codec, so this is what tells
// an audio traf apart from a video one.
func ParseTkhd(payload []byte) (trackID uint32, err error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("tkhd: %d bytes: %w", len(payload), errParse)
	}
	// version/flags(4), creation_time, modification_time, track_ID. The two time
	// fields are 4 bytes each in version 0 and 8 bytes each in version 1, which
	// moves track_ID; a reserved version has an unknown layout, so reject it rather
	// than read track_ID from the wrong offset.
	switch payload[0] {
	case 1:
		if len(payload) < 24 {
			return 0, fmt.Errorf("tkhd v1: %d bytes, need 24: %w", len(payload), errParse)
		}
		return binary.BigEndian.Uint32(payload[20:]), nil
	case 0:
		if len(payload) < 16 {
			return 0, fmt.Errorf("tkhd v0: %d bytes, need 16: %w", len(payload), errParse)
		}
		return binary.BigEndian.Uint32(payload[12:]), nil
	default:
		return 0, fmt.Errorf("tkhd version %d unsupported: %w", payload[0], errParse)
	}
}

// TrunSample is one decoded per-sample record from a trun run. A field absent
// from the box body (its trun flag clear) is left zero; the caller resolves an
// absent size or duration against the tfhd and trex defaults.
type TrunSample struct {
	Duration          uint32
	Size              uint32
	Flags             uint32
	CompositionOffset int64
}

// Trun holds one decoded track fragment run (trun). The Has* flags report which
// per-sample fields the box actually carried, so the caller knows whether to read
// a sample's size and duration from Samples or from the tfhd/trex defaults. When
// no per-sample field is present, Samples is nil and SampleCount alone describes
// the run.
type Trun struct {
	SampleCount        uint32
	HasDataOffset      bool
	DataOffset         int32
	HasSampleDuration  bool
	HasSampleSize      bool
	HasSampleFlags     bool
	HasCompositionTime bool
	Samples            []TrunSample
}

// ParseTrun decodes a trun (track fragment run) FullBox body. The flags select
// the optional fields after sample_count (data_offset, first_sample_flags) and
// the per-sample record layout (duration, size, flags, composition_time_offset),
// each 4 bytes. The per-record width times sample_count is bounded against the
// remaining body before any per-sample slice is allocated, so a hostile
// sample_count cannot force a large allocation. first_sample_flags and the
// per-sample flags are skipped: audio access units are all sync samples and their
// flags do not affect extraction.
func ParseTrun(payload []byte) (Trun, error) {
	if len(payload) < 8 {
		return Trun{}, fmt.Errorf("trun: %d bytes, need 8: %w", len(payload), errParse)
	}
	version := payload[0]
	if version > 1 {
		// Only versions 0 and 1 are defined (they differ only in the composition
		// offset's sign); a reserved version signals a layout this parser does not
		// understand, so reject it.
		return Trun{}, fmt.Errorf("trun version %d unsupported: %w", version, errParse)
	}
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	var t Trun
	t.SampleCount = binary.BigEndian.Uint32(payload[4:])

	p := 8
	t.HasDataOffset = flags&trunDataOffsetPresent != 0
	if t.HasDataOffset {
		if p+4 > len(payload) {
			return Trun{}, fmt.Errorf("trun data_offset overruns %d bytes: %w", len(payload), errParse)
		}
		t.DataOffset = int32(binary.BigEndian.Uint32(payload[p:]))
		p += 4
	}
	if flags&trunFirstSampleFlagsPresent != 0 {
		if p+4 > len(payload) {
			return Trun{}, fmt.Errorf("trun first_sample_flags overruns %d bytes: %w", len(payload), errParse)
		}
		p += 4 // first_sample_flags: sync-sample flags, not needed for audio
	}

	t.HasSampleDuration = flags&trunSampleDurationPresent != 0
	t.HasSampleSize = flags&trunSampleSizePresent != 0
	t.HasSampleFlags = flags&trunSampleFlagsPresent != 0
	t.HasCompositionTime = flags&trunSampleCompositionTimeOffsetsPresent != 0

	recordWidth := 0
	if t.HasSampleDuration {
		recordWidth += 4
	}
	if t.HasSampleSize {
		recordWidth += 4
	}
	if t.HasSampleFlags {
		recordWidth += 4
	}
	if t.HasCompositionTime {
		recordWidth += 4
	}

	// Bound sample_count against the remaining body before allocating. recordWidth
	// is at most 16 and SampleCount at most 2^32-1, so the product stays well
	// inside int64 and cannot wrap.
	remaining := int64(len(payload) - p)
	if int64(recordWidth)*int64(t.SampleCount) > remaining {
		return Trun{}, fmt.Errorf("trun sample_count %d records overrun %d bytes: %w", t.SampleCount, remaining, errParse)
	}
	// No per-sample fields: the run is fully described by sample_count and the
	// tfhd/trex defaults the caller applies. Leave Samples nil.
	if recordWidth == 0 {
		return t, nil
	}

	t.Samples = make([]TrunSample, t.SampleCount)
	for i := range t.Samples {
		var s TrunSample
		if t.HasSampleDuration {
			s.Duration = binary.BigEndian.Uint32(payload[p:])
			p += 4
		}
		if t.HasSampleSize {
			s.Size = binary.BigEndian.Uint32(payload[p:])
			p += 4
		}
		if t.HasSampleFlags {
			s.Flags = binary.BigEndian.Uint32(payload[p:])
			p += 4
		}
		if t.HasCompositionTime {
			raw := binary.BigEndian.Uint32(payload[p:])
			// Version 0 encodes the composition offset as unsigned, version 1 as
			// signed. The demuxer does not use it for audio, but decode it faithfully.
			if version == 0 {
				s.CompositionOffset = int64(raw)
			} else {
				s.CompositionOffset = int64(int32(raw))
			}
			p += 4
		}
		t.Samples[i] = s
	}
	return t, nil
}
