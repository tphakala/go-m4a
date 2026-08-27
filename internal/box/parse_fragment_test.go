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
	if len(got.Samples) != 3 {
		t.Fatalf("len(Samples) = %d, want 3", len(got.Samples))
	}
	for i, s := range got.Samples {
		if s.Size != sizes[i] {
			t.Errorf("Samples[%d].Size = %d, want %d", i, s.Size, sizes[i])
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
	for i := range got.Samples {
		if got.Samples[i].Size != sizes[i] || got.Samples[i].Duration != durs[i] {
			t.Errorf("Samples[%d] = %+v, want size=%d dur=%d", i, got.Samples[i], sizes[i], durs[i])
		}
	}
}

// TestParseTrunNoPerSampleFields builds a trun with only data_offset present and
// no per-sample field flags, so sample_count alone describes the run and Samples
// is nil. The demuxer resolves every size from tfhd/trex defaults in this case.
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
	if got.SampleCount != 3 || got.Samples != nil {
		t.Errorf("SampleCount=%d Samples=%v, want 3/nil", got.SampleCount, got.Samples)
	}
	if got.HasSampleSize {
		t.Errorf("HasSampleSize = true, want false")
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
