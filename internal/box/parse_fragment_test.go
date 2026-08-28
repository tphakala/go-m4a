// SPDX-License-Identifier: MIT

package box

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// body strips the 8-byte box header AppendX emits, returning the FullBox body the
// ParseX functions consume. Every fragment box here uses the 32-bit header form.
func body(box []byte) []byte {
	return box[8:]
}

// fullBox builds a raw FullBox body (version/flags then payload) for hand-built
// edge cases the append side never emits.
func fullBox(version uint8, flags uint32, payload []byte) []byte {
	b := make([]byte, 0, 4+len(payload))
	b = append(b, version, byte(flags>>16), byte(flags>>8), byte(flags))
	return append(b, payload...)
}

func TestParseTfhdRoundTrip(t *testing.T) {
	t.Parallel()
	for _, dur := range []uint32{0, 1024} {
		box := AppendTfhd(nil, 1, dur)
		got, err := ParseTfhd(body(box))
		if err != nil {
			t.Fatalf("ParseTfhd(dur=%d): %v", dur, err)
		}
		if got.TrackID != 1 {
			t.Errorf("dur=%d TrackID = %d, want 1", dur, got.TrackID)
		}
		if !got.DefaultBaseIsMoof {
			t.Errorf("dur=%d DefaultBaseIsMoof = false, want true", dur)
		}
		if got.HasBaseDataOffset {
			t.Errorf("dur=%d HasBaseDataOffset = true, want false", dur)
		}
		wantHasDur := dur != 0
		if got.HasDefaultSampleDuration != wantHasDur {
			t.Errorf("dur=%d HasDefaultSampleDuration = %v, want %v", dur, got.HasDefaultSampleDuration, wantHasDur)
		}
		if got.DefaultSampleDuration != dur {
			t.Errorf("dur=%d DefaultSampleDuration = %d, want %d", dur, got.DefaultSampleDuration, dur)
		}
	}
}

func TestParseTfhdAllOptionalFields(t *testing.T) {
	t.Parallel()
	flags := uint32(tfhdBaseDataOffsetPresent | tfhdSampleDescriptionIndexPresent |
		tfhdDefaultSampleDurationPresent | tfhdDefaultSampleSizePresent | tfhdDefaultSampleFlagsPresent)
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 7)               // track_ID
	p = binary.BigEndian.AppendUint64(p, 0x1_0000_0002)   // base_data_offset (>32-bit)
	p = binary.BigEndian.AppendUint32(p, 1)               // sample_description_index (skipped)
	p = binary.BigEndian.AppendUint32(p, 960)             // default_sample_duration
	p = binary.BigEndian.AppendUint32(p, 333)             // default_sample_size
	p = binary.BigEndian.AppendUint32(p, SyncSampleFlags) // default_sample_flags
	got, err := ParseTfhd(fullBox(0, flags, p))
	if err != nil {
		t.Fatalf("ParseTfhd: %v", err)
	}
	if got.TrackID != 7 {
		t.Errorf("TrackID = %d, want 7", got.TrackID)
	}
	if !got.HasBaseDataOffset || got.BaseDataOffset != 0x1_0000_0002 {
		t.Errorf("BaseDataOffset = %#x (has=%v), want 0x100000002", got.BaseDataOffset, got.HasBaseDataOffset)
	}
	if got.DefaultBaseIsMoof {
		t.Errorf("DefaultBaseIsMoof = true, want false (flag not set)")
	}
	if !got.HasDefaultSampleDuration || got.DefaultSampleDuration != 960 {
		t.Errorf("DefaultSampleDuration = %d (has=%v), want 960", got.DefaultSampleDuration, got.HasDefaultSampleDuration)
	}
	if !got.HasDefaultSampleSize || got.DefaultSampleSize != 333 {
		t.Errorf("DefaultSampleSize = %d (has=%v), want 333", got.DefaultSampleSize, got.HasDefaultSampleSize)
	}
	if !got.HasDefaultSampleFlags || got.DefaultSampleFlags != SyncSampleFlags {
		t.Errorf("DefaultSampleFlags = %#x (has=%v), want %#x", got.DefaultSampleFlags, got.HasDefaultSampleFlags, SyncSampleFlags)
	}
}

func TestParseTfhdTruncated(t *testing.T) {
	t.Parallel()
	// default_sample_size flag set but no bytes for it.
	b := fullBox(0, tfhdDefaultSampleSizePresent, binary.BigEndian.AppendUint32(nil, 3))
	if _, err := ParseTfhd(b); !errors.Is(err, errParse) {
		t.Fatalf("ParseTfhd truncated = %v, want errParse", err)
	}
	if _, err := ParseTfhd([]byte{0, 0, 0}); !errors.Is(err, errParse) {
		t.Fatalf("ParseTfhd short header = %v, want errParse", err)
	}
}

func TestParseTfdtRoundTripV1(t *testing.T) {
	t.Parallel()
	box := AppendTfdt(nil, 0x1_2345_6789)
	got, err := ParseTfdt(body(box))
	if err != nil {
		t.Fatalf("ParseTfdt: %v", err)
	}
	if got != 0x1_2345_6789 {
		t.Errorf("baseMediaDecodeTime = %#x, want 0x123456789", got)
	}
}

func TestParseTfdtV0(t *testing.T) {
	t.Parallel()
	b := fullBox(0, 0, binary.BigEndian.AppendUint32(nil, 48000))
	got, err := ParseTfdt(b)
	if err != nil {
		t.Fatalf("ParseTfdt v0: %v", err)
	}
	if got != 48000 {
		t.Errorf("baseMediaDecodeTime = %d, want 48000", got)
	}
}

func TestParseTfdtTruncated(t *testing.T) {
	t.Parallel()
	if _, err := ParseTfdt(fullBox(1, 0, []byte{0, 0, 0, 0})); !errors.Is(err, errParse) {
		t.Fatalf("ParseTfdt v1 truncated = %v, want errParse", err)
	}
	if _, err := ParseTfdt([]byte{0, 0}); !errors.Is(err, errParse) {
		t.Fatalf("ParseTfdt short = %v, want errParse", err)
	}
}

func TestParseTrexRoundTrip(t *testing.T) {
	t.Parallel()
	box := AppendTrex(nil, 1, 1024, SyncSampleFlags)
	got, err := ParseTrex(body(box))
	if err != nil {
		t.Fatalf("ParseTrex: %v", err)
	}
	want := Trex{TrackID: 1, DefaultSampleDescriptionIndex: 1, DefaultSampleDuration: 1024, DefaultSampleSize: 0, DefaultSampleFlags: SyncSampleFlags}
	if got != want {
		t.Errorf("ParseTrex = %+v, want %+v", got, want)
	}
}

func TestParseTrexTruncated(t *testing.T) {
	t.Parallel()
	if _, err := ParseTrex(make([]byte, 23)); !errors.Is(err, errParse) {
		t.Fatalf("ParseTrex short = %v, want errParse", err)
	}
}

func TestParseTrunRoundTripSizesOnly(t *testing.T) {
	t.Parallel()
	sizes := []uint32{100, 200, 300}
	box := AppendTrun(nil, 1234, sizes, nil)
	got, err := ParseTrun(body(box))
	if err != nil {
		t.Fatalf("ParseTrun: %v", err)
	}
	if got.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", got.SampleCount)
	}
	if !got.HasDataOffset || got.DataOffset != 1234 {
		t.Errorf("DataOffset = %d (has=%v), want 1234", got.DataOffset, got.HasDataOffset)
	}
	if !got.HasSampleSize || got.HasSampleDuration {
		t.Errorf("HasSampleSize=%v HasSampleDuration=%v, want true/false", got.HasSampleSize, got.HasSampleDuration)
	}
	for i := range sizes {
		if got.SampleSize(i) != sizes[i] {
			t.Errorf("SampleSize(%d) = %d, want %d", i, got.SampleSize(i), sizes[i])
		}
		// The run carries no durations, so the accessor reports 0 and the caller
		// falls back to the tfhd or trex default.
		if got.SampleDuration(i) != 0 {
			t.Errorf("SampleDuration(%d) = %d, want 0 for a run without durations", i, got.SampleDuration(i))
		}
	}
}

func TestParseTrunRoundTripWithDurations(t *testing.T) {
	t.Parallel()
	sizes := []uint32{50, 60}
	durs := []uint32{960, 1024}
	box := AppendTrun(nil, -8, sizes, durs)
	got, err := ParseTrun(body(box))
	if err != nil {
		t.Fatalf("ParseTrun: %v", err)
	}
	if got.DataOffset != -8 {
		t.Errorf("DataOffset = %d, want -8", got.DataOffset)
	}
	if !got.HasSampleSize || !got.HasSampleDuration {
		t.Errorf("HasSampleSize=%v HasSampleDuration=%v, want true/true", got.HasSampleSize, got.HasSampleDuration)
	}
	for i := range sizes {
		if got.SampleSize(i) != sizes[i] || got.SampleDuration(i) != durs[i] {
			t.Errorf("sample %d = size %d dur %d, want size=%d dur=%d",
				i, got.SampleSize(i), got.SampleDuration(i), sizes[i], durs[i])
		}
	}
}

// TestParseTrunNoPerSampleFields builds a trun with only data_offset present and
// no per-sample field flags, so sample_count alone describes the run and there
// are no records to read. The demuxer resolves every size from tfhd/trex defaults
// in this case, and every accessor must report 0 rather than index a record that
// is not there.
func TestParseTrunNoPerSampleFields(t *testing.T) {
	t.Parallel()
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 3)  // sample_count
	p = binary.BigEndian.AppendUint32(p, 16) // data_offset
	b := fullBox(0, trunDataOffsetPresent, p)
	got, err := ParseTrun(b)
	if err != nil {
		t.Fatalf("ParseTrun: %v", err)
	}
	if got.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", got.SampleCount)
	}
	if got.HasSampleSize {
		t.Errorf("HasSampleSize = true, want false")
	}
	// A zero-width record means no records at all, so every accessor answers from
	// the absent-field path instead of dividing into an empty slice.
	for i := range int(got.SampleCount) {
		if got.SampleSize(i) != 0 || got.SampleDuration(i) != 0 || got.SampleFlags(i) != 0 {
			t.Errorf("sample %d = size %d dur %d flags %d, want all 0",
				i, got.SampleSize(i), got.SampleDuration(i), got.SampleFlags(i))
		}
		if got.SampleCompositionOffset(i) != 0 {
			t.Errorf("SampleCompositionOffset(%d) = %d, want 0", i, got.SampleCompositionOffset(i))
		}
	}
}

// TestParseTrunAllFieldsPresent builds a run carrying every per-sample field, the
// widest record layout, and reads each one back. It is the test that pins the
// field order inside a record (duration, size, flags, composition_time_offset):
// a wrong offset silently returns a neighbouring field rather than failing.
func TestParseTrunAllFieldsPresent(t *testing.T) {
	t.Parallel()
	const flags = trunDataOffsetPresent | trunSampleDurationPresent |
		trunSampleSizePresent | trunSampleFlagsPresent | trunSampleCompositionTimeOffsetsPresent
	type record struct {
		dur, size, flags uint32
		composition      int32
	}
	records := []record{
		{dur: 960, size: 11, flags: 0x0100_0000, composition: 5},
		{dur: 1024, size: 22, flags: 0x0200_0000, composition: -7},
	}
	var p []byte
	p = binary.BigEndian.AppendUint32(p, uint32(len(records)))
	p = binary.BigEndian.AppendUint32(p, 64) // data_offset
	for _, r := range records {
		p = binary.BigEndian.AppendUint32(p, r.dur)
		p = binary.BigEndian.AppendUint32(p, r.size)
		p = binary.BigEndian.AppendUint32(p, r.flags)
		p = binary.BigEndian.AppendUint32(p, uint32(r.composition))
	}

	// Version 1 signs the composition offset, so the negative one round-trips.
	got, err := ParseTrun(fullBox(1, flags, p))
	if err != nil {
		t.Fatalf("ParseTrun: %v", err)
	}
	if !got.HasSampleDuration || !got.HasSampleSize || !got.HasSampleFlags || !got.HasCompositionTime {
		t.Fatalf("Has* = %v/%v/%v/%v, want all true",
			got.HasSampleDuration, got.HasSampleSize, got.HasSampleFlags, got.HasCompositionTime)
	}
	for i, want := range records {
		if got.SampleDuration(i) != want.dur {
			t.Errorf("SampleDuration(%d) = %d, want %d", i, got.SampleDuration(i), want.dur)
		}
		if got.SampleSize(i) != want.size {
			t.Errorf("SampleSize(%d) = %d, want %d", i, got.SampleSize(i), want.size)
		}
		if got.SampleFlags(i) != want.flags {
			t.Errorf("SampleFlags(%d) = %#x, want %#x", i, got.SampleFlags(i), want.flags)
		}
		if got.SampleCompositionOffset(i) != int64(want.composition) {
			t.Errorf("SampleCompositionOffset(%d) = %d, want %d", i, got.SampleCompositionOffset(i), want.composition)
		}
	}

	// The same bytes read as version 0 take the composition offset as unsigned, so
	// the negative record reads back as its two's complement.
	v0, err := ParseTrun(fullBox(0, flags, p))
	if err != nil {
		t.Fatalf("ParseTrun v0: %v", err)
	}
	if want := int64(uint32(records[1].composition)); v0.SampleCompositionOffset(1) != want {
		t.Errorf("v0 SampleCompositionOffset(1) = %d, want %d", v0.SampleCompositionOffset(1), want)
	}
}

// TestParseTrunRecordsExcludeTrailingBytes checks that a run retains only its own
// records and not whatever follows them in the buffer. A trun body sized past its
// records is legal padding, and an accessor must not be able to read into it.
func TestParseTrunRecordsExcludeTrailingBytes(t *testing.T) {
	t.Parallel()
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 1)           // sample_count
	p = binary.BigEndian.AppendUint32(p, 0xDEAD_BEEF) // the one size record
	p = append(p, 0xFF, 0xFF, 0xFF, 0xFF)             // trailing bytes past the records
	got, err := ParseTrun(fullBox(0, trunSampleSizePresent, p))
	if err != nil {
		t.Fatalf("ParseTrun: %v", err)
	}
	if got.SampleSize(0) != 0xDEAD_BEEF {
		t.Errorf("SampleSize(0) = %#x, want 0xDEADBEEF", got.SampleSize(0))
	}
	if len(got.records) != 4 {
		t.Errorf("len(records) = %d, want 4 (the trailing bytes must not be retained)", len(got.records))
	}
}

// TestParseTrunSampleCountOverrun sets a huge sample_count with no room for the
// records, which must be rejected before any allocation.
func TestParseTrunSampleCountOverrun(t *testing.T) {
	t.Parallel()
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0xFFFF_FFFF) // sample_count
	p = binary.BigEndian.AppendUint32(p, 16)          // data_offset
	p = binary.BigEndian.AppendUint32(p, 100)         // one size record only
	b := fullBox(0, trunDataOffsetPresent|trunSampleSizePresent, p)
	if _, err := ParseTrun(b); !errors.Is(err, errParse) {
		t.Fatalf("ParseTrun overrun = %v, want errParse", err)
	}
}

func TestParseTrunTruncated(t *testing.T) {
	t.Parallel()
	if _, err := ParseTrun([]byte{0, 0, 0, 0, 0}); !errors.Is(err, errParse) {
		t.Fatalf("ParseTrun short = %v, want errParse", err)
	}
}

func TestParseTkhdRoundTrip(t *testing.T) {
	t.Parallel()
	// AppendTkhd emits version 0 for a duration within uint32 and version 1 above
	// it; both carry track_ID, at different offsets. Exercise both widths.
	v0 := AppendTkhd(nil, 7, 48000)
	if got, err := ParseTkhd(body(v0)); err != nil || got != 7 {
		t.Fatalf("ParseTkhd v0 = %d, %v; want 7, nil", got, err)
	}
	v1 := AppendTkhd(nil, 9, math.MaxUint32+1)
	if got, err := ParseTkhd(body(v1)); err != nil || got != 9 {
		t.Fatalf("ParseTkhd v1 = %d, %v; want 9, nil", got, err)
	}
}

func TestParseTkhdTruncated(t *testing.T) {
	t.Parallel()
	if _, err := ParseTkhd([]byte{0, 0, 0}); !errors.Is(err, errParse) {
		t.Fatalf("ParseTkhd short = %v, want errParse", err)
	}
	// version 1 declared but the body is too short for the 64-bit time fields.
	if _, err := ParseTkhd(fullBox(1, 0, make([]byte, 8))); !errors.Is(err, errParse) {
		t.Fatalf("ParseTkhd truncated v1 = %v, want errParse", err)
	}
	// version 0 with fewer than the 16 bytes track_ID needs.
	if _, err := ParseTkhd(fullBox(0, 0, make([]byte, 6))); !errors.Is(err, errParse) {
		t.Fatalf("ParseTkhd truncated v0 = %v, want errParse", err)
	}
}

// TestParseFragmentReservedVersions checks that the version-dependent parsers
// reject a reserved FullBox version rather than decoding it with a guessed layout.
func TestParseFragmentReservedVersions(t *testing.T) {
	t.Parallel()
	if _, err := ParseTfdt(fullBox(2, 0, make([]byte, 8))); !errors.Is(err, errParse) {
		t.Errorf("ParseTfdt v2 = %v, want errParse", err)
	}
	if _, err := ParseTkhd(fullBox(2, 0, make([]byte, 20))); !errors.Is(err, errParse) {
		t.Errorf("ParseTkhd v2 = %v, want errParse", err)
	}
	if _, err := ParseTrun(fullBox(2, trunDataOffsetPresent, make([]byte, 8))); !errors.Is(err, errParse) {
		t.Errorf("ParseTrun v2 = %v, want errParse", err)
	}
}
