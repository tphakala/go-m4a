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

// Trun holds one decoded track fragment run (trun). The Has* flags report which
// per-sample fields the box actually carried, so the caller knows whether to read
// a sample's size and duration from the accessors or from the tfhd/trex defaults.
// When no per-sample field is present, SampleCount alone describes the run.
//
// The per-sample records are not decoded into a slice. A run in a CMAF fragment
// routinely declares hundreds of samples, of which the demuxer reads only the
// size and duration, so Trun retains the record bytes of the payload it was
// parsed from and each accessor decodes one field on demand. That keeps ParseTrun
// allocation-free however many samples a run declares.
//
// Retaining those bytes means a Trun aliases the buffer it was parsed from and
// must not outlive it. The demuxer parses and consumes each run inside the walk
// over the moof body it was read from, which is what makes this safe. Anything
// that keeps a Trun past that walk, or hands one to a later phase, has to copy
// the fields it needs first, or it pins the whole fragment body.
type Trun struct {
	SampleCount        uint32
	HasDataOffset      bool
	DataOffset         int32
	HasSampleDuration  bool
	HasSampleSize      bool
	HasSampleFlags     bool
	HasCompositionTime bool

	// version selects how a composition offset is signed: unsigned in version 0,
	// signed in version 1.
	version uint8

	// recordWidth is one per-sample record's byte length and the *Off fields are
	// each field's position inside that record, or -1 when the run omits it. They
	// are computed once here rather than rediscovered from the Has* flags on every
	// accessor call, which makes reading a field one multiply and one load.
	recordWidth    int
	durationOff    int
	sizeOff        int
	flagsOff       int
	compositionOff int

	// records is exactly recordWidth*SampleCount bytes of per-sample records
	// aliasing the parsed payload, and nil when recordWidth is 0. ParseTrun proves
	// that length fits the box body before slicing it, which is what makes every
	// accessor index in range for a sample below SampleCount.
	records []byte
}

// SampleDuration returns sample i's duration in media-timescale ticks, or 0 when
// the run carries no per-sample durations (HasSampleDuration false), which the
// caller resolves against the tfhd and trex defaults instead. Like a slice index,
// an i outside [0, SampleCount) panics.
func (t *Trun) SampleDuration(i int) uint32 { return t.field(i, t.durationOff) }

// SampleSize returns sample i's byte length, or 0 when the run carries no
// per-sample sizes (HasSampleSize false). Indexing follows SampleDuration.
func (t *Trun) SampleSize(i int) uint32 { return t.field(i, t.sizeOff) }

// SampleFlags returns sample i's flags word, or 0 when the run carries no
// per-sample flags. Audio access units are all sync samples, so the demuxer does
// not consult this; it is here so a caller inspecting a foreign fragment can.
func (t *Trun) SampleFlags(i int) uint32 { return t.field(i, t.flagsOff) }

// SampleCompositionOffset returns sample i's composition time offset in
// media-timescale ticks, or 0 when the run carries none. A version 0 run encodes
// it unsigned and a version 1 run signed, which this reads faithfully even though
// the demuxer has no use for it on an audio track.
func (t *Trun) SampleCompositionOffset(i int) int64 {
	if t.compositionOff < 0 {
		return 0
	}
	raw := binary.BigEndian.Uint32(t.records[i*t.recordWidth+t.compositionOff:])
	if t.version == 0 {
		return int64(raw)
	}
	return int64(int32(raw))
}

// field reads the 4-byte field at off within sample i's record, reporting 0 for
// an absent field (off < 0) so an accessor never has to special-case one.
func (t *Trun) field(i, off int) uint32 {
	if off < 0 {
		return 0
	}
	return binary.BigEndian.Uint32(t.records[i*t.recordWidth+off:])
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

	// Lay out one per-sample record: the present fields appear in this ISO order,
	// each 4 bytes. An absent field gets offset -1 so an accessor can tell it apart
	// from one that genuinely sits at the front of the record.
	t.version = version
	t.durationOff, t.sizeOff, t.flagsOff, t.compositionOff = -1, -1, -1, -1
	recordWidth := 0
	if t.HasSampleDuration {
		t.durationOff = recordWidth
		recordWidth += 4
	}
	if t.HasSampleSize {
		t.sizeOff = recordWidth
		recordWidth += 4
	}
	if t.HasSampleFlags {
		t.flagsOff = recordWidth
		recordWidth += 4
	}
	if t.HasCompositionTime {
		t.compositionOff = recordWidth
		recordWidth += 4
	}
	t.recordWidth = recordWidth

	// Bound sample_count against the remaining body before retaining anything.
	// recordWidth is at most 16 and SampleCount at most 2^32-1, so the product
	// stays well inside int64 and cannot wrap. Passing this bound is also what
	// proves every accessor index is in range: remaining is a slice length, so a
	// product no larger than it fits an int on a 32-bit build too.
	remaining := int64(len(payload) - p)
	recordBytes := int64(recordWidth) * int64(t.SampleCount)
	if recordBytes > remaining {
		return Trun{}, fmt.Errorf("trun sample_count %d records overrun %d bytes: %w", t.SampleCount, remaining, errParse)
	}
	// No per-sample fields: the run is fully described by sample_count and the
	// tfhd/trex defaults the caller applies. Leave records nil.
	if recordWidth == 0 {
		return t, nil
	}
	// Retain exactly the records, not the rest of the box body behind them: a trun
	// may be followed by other boxes in the same buffer, and the tighter slice
	// keeps an accessor's index arithmetic inside what was actually validated.
	t.records = payload[p : p+int(recordBytes)]
	return t, nil
}
