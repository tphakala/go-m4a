// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"testing"
)

func TestAppendStyp(t *testing.T) {
	got := AppendStyp(nil, NewFourCC("msdh"), 0, NewFourCC("msdh"), NewFourCC("cmfs"))
	want := mustHex(t, "00000018"+"73747970"+"6d736468"+"00000000"+"6d736468"+"636d6673")
	if !bytes.Equal(got, want) {
		t.Fatalf("styp = % x, want % x", got, want)
	}
}

func TestAppendMfhd(t *testing.T) {
	got := AppendMfhd(nil, 1)
	want := mustHex(t, "00000010"+"6d666864"+"00000000"+"00000001")
	if !bytes.Equal(got, want) {
		t.Fatalf("mfhd = % x, want % x", got, want)
	}
	if len(got) != MfhdSize {
		t.Fatalf("mfhd length %d, want MfhdSize %d", len(got), MfhdSize)
	}
}

func TestAppendTfdtIsAlways64Bit(t *testing.T) {
	// Version 1 unconditionally, even for a decode time that fits 32 bits: a live
	// stream at 48 kHz wraps a 32-bit field after about 24.8 hours.
	got := AppendTfdt(nil, 0)
	want := mustHex(t, "00000014"+"74666474"+"01000000"+"0000000000000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("tfdt(0) = % x, want % x", got, want)
	}
	if len(got) != TfdtSize {
		t.Fatalf("tfdt length %d, want TfdtSize %d", len(got), TfdtSize)
	}

	// A decode time beyond 32 bits round-trips intact.
	const beyond32 = uint64(1) << 40
	got = AppendTfdt(nil, beyond32)
	want = mustHex(t, "00000014"+"74666474"+"01000000"+"0000010000000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("tfdt(2^40) = % x, want % x", got, want)
	}
}

func TestAppendTfhd(t *testing.T) {
	// With a fragment-wide sample duration: default-base-is-moof plus
	// default-sample-duration-present (0x020008).
	got := AppendTfhd(nil, 1, 1024)
	want := mustHex(t, "00000014"+"74666864"+"00020008"+"00000001"+"00000400")
	if !bytes.Equal(got, want) {
		t.Fatalf("tfhd(dur=1024) = % x, want % x", got, want)
	}

	// Without one: default-base-is-moof only, and the duration field is absent.
	got = AppendTfhd(nil, 1, 0)
	want = mustHex(t, "00000010"+"74666864"+"00020000"+"00000001")
	if !bytes.Equal(got, want) {
		t.Fatalf("tfhd(dur=0) = % x, want % x", got, want)
	}
}

// TestTfhdNeverSetsBaseDataOffset guards the CMAF constraint: sample data must be
// addressed relative to the enclosing moof, so default-base-is-moof is always set
// and base-data-offset-present never is.
func TestTfhdNeverSetsBaseDataOffset(t *testing.T) {
	for _, dur := range []uint32{0, 1024} {
		b := AppendTfhd(nil, 1, dur)
		flags := uint32(b[9])<<16 | uint32(b[10])<<8 | uint32(b[11])
		if flags&tfhdDefaultBaseIsMoof == 0 {
			t.Errorf("tfhd(dur=%d) flags %#06x: default-base-is-moof not set", dur, flags)
		}
		if flags&0x000001 != 0 {
			t.Errorf("tfhd(dur=%d) flags %#06x: base-data-offset-present is forbidden in CMAF", dur, flags)
		}
	}
}

func TestAppendTrex(t *testing.T) {
	got := AppendTrex(nil, 1, 1024, SyncSampleFlags)
	want := mustHex(t, "00000020"+"74726578"+"00000000"+
		"00000001"+"00000001"+"00000400"+"00000000"+"02000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("trex = % x, want % x", got, want)
	}
}

// TestSyncSampleFlags pins the sample_flags encoding: sample_depends_on = 2 in
// bits 24-25 marks an independently decodable sample, and
// sample_is_non_sync_sample (bit 16) stays clear.
func TestSyncSampleFlags(t *testing.T) {
	if dependsOn := (SyncSampleFlags >> 24) & 0x3; dependsOn != 2 {
		t.Errorf("sample_depends_on = %d, want 2", dependsOn)
	}
	if nonSync := (SyncSampleFlags >> 16) & 0x1; nonSync != 0 {
		t.Errorf("sample_is_non_sync_sample = %d, want 0", nonSync)
	}
}

func TestAppendTrun(t *testing.T) {
	// Durations omitted: flags are data-offset-present | sample-size-present.
	got := AppendTrun(nil, 484, []uint32{294, 351}, nil)
	want := mustHex(t, "0000001c"+"7472756e"+"00000201"+
		"00000002"+"000001e4"+"00000126"+"0000015f")
	if !bytes.Equal(got, want) {
		t.Fatalf("trun (no durations) = % x, want % x", got, want)
	}

	// Durations present: the flag is added and each record is duration then size,
	// in that order.
	got = AppendTrun(nil, 100, []uint32{294, 351}, []uint32{1024, 512})
	want = mustHex(t, "00000024"+"7472756e"+"00000301"+
		"00000002"+"00000064"+
		"00000400"+"00000126"+
		"00000200"+"0000015f")
	if !bytes.Equal(got, want) {
		t.Fatalf("trun (with durations) = % x, want % x", got, want)
	}
}

// TestFragmentSizePredictions is the invariant the whole segment writer rests on:
// trun's data_offset is measured from the start of the enclosing moof, so the moof
// length has to be known before any of it is emitted. If a predictor ever drifts
// from what the marshaler actually writes, every segment gets a wrong data_offset
// and players read garbage, so pin them against real output.
func TestFragmentSizePredictions(t *testing.T) {
	for _, n := range []int{0, 1, 2, 94, 1000} {
		sizes := make([]uint32, n)
		durations := make([]uint32, n)
		for i := range sizes {
			sizes[i] = uint32(i + 1)
			durations[i] = 1024
		}

		if got, want := len(AppendTrun(nil, 0, sizes, nil)), TrunSize(n, false); got != want {
			t.Errorf("n=%d: trun without durations is %d bytes, TrunSize says %d", n, got, want)
		}
		if got, want := len(AppendTrun(nil, 0, sizes, durations)), TrunSize(n, true); got != want {
			t.Errorf("n=%d: trun with durations is %d bytes, TrunSize says %d", n, got, want)
		}
	}

	for _, dur := range []uint32{0, 1024} {
		if got, want := len(AppendTfhd(nil, 1, dur)), TfhdSize(dur); got != want {
			t.Errorf("dur=%d: tfhd is %d bytes, TfhdSize says %d", dur, got, want)
		}
	}
	if got := len(AppendMfhd(nil, 1)); got != MfhdSize {
		t.Errorf("mfhd is %d bytes, MfhdSize says %d", got, MfhdSize)
	}
	if got := len(AppendTfdt(nil, 0)); got != TfdtSize {
		t.Errorf("tfdt is %d bytes, TfdtSize says %d", got, TfdtSize)
	}
	if got := len(AppendMoofHeader(nil, 0)); got != MoofHeaderSize {
		t.Errorf("moof header is %d bytes, MoofHeaderSize says %d", got, MoofHeaderSize)
	}
	if got := len(AppendTrafHeader(nil, 0)); got != TrafHeaderSize {
		t.Errorf("traf header is %d bytes, TrafHeaderSize says %d", got, TrafHeaderSize)
	}
}

// TestAppendStscEntriesEmpty covers the fragmented init segment's sample tables,
// which must be structurally present but carry no entries.
func TestAppendStscEntriesEmpty(t *testing.T) {
	want := mustHex(t, "00000010"+"73747363"+"00000000"+"00000000")
	for _, entries := range [][]StscEntry{nil, {}} {
		got := AppendStscEntries(nil, entries)
		if !bytes.Equal(got, want) {
			t.Fatalf("empty stsc = % x, want % x", got, want)
		}
	}
}

// TestAppendStscUnchanged pins that routing AppendStsc through AppendStscEntries
// did not alter the bytes the non-fragmented writer emits.
func TestAppendStscUnchanged(t *testing.T) {
	got := AppendStsc(nil, 1, 8, 1)
	want := mustHex(t, "0000001c"+"73747363"+"00000000"+"00000001"+"00000001"+"00000008"+"00000001")
	if !bytes.Equal(got, want) {
		t.Fatalf("stsc = % x, want % x", got, want)
	}
}

// TestAppendMvexWrapsChildren checks the mvex container computes its own size
// from the children it wraps.
func TestAppendMvexWrapsChildren(t *testing.T) {
	child := []byte{1, 2, 3, 4}
	got := AppendMvex(nil, child)
	if len(got) != 8+len(child) {
		t.Errorf("mvex length %d, want %d", len(got), 8+len(child))
	}
	if string(got[4:8]) != "mvex" {
		t.Errorf("mvex type = %q, want %q", got[4:8], "mvex")
	}
	if !bytes.Equal(got[8:], child) {
		t.Errorf("mvex children = % x, want % x", got[8:], child)
	}
}

// TestFragmentContainerHeaders checks the header-only assemblers, which take a
// size the caller predicted so the children can follow directly in one buffer.
func TestFragmentContainerHeaders(t *testing.T) {
	if got, want := AppendMoofHeader(nil, 476), mustHex(t, "000001dc"+"6d6f6f66"); !bytes.Equal(got, want) {
		t.Errorf("moof header = % x, want % x", got, want)
	}
	if got, want := AppendTrafHeader(nil, 452), mustHex(t, "000001c4"+"74726166"); !bytes.Equal(got, want) {
		t.Errorf("traf header = % x, want % x", got, want)
	}
}

// TestAppendMdat checks the plain 32-bit mdat box a media segment carries.
func TestAppendMdat(t *testing.T) {
	got := AppendMdat(nil, []byte{0xde, 0xad, 0xbe, 0xef})
	want := mustHex(t, "0000000c"+"6d646174"+"deadbeef")
	if !bytes.Equal(got, want) {
		t.Fatalf("mdat = % x, want % x", got, want)
	}
	if len(got)-4 != MdatShortHeaderSize {
		t.Errorf("mdat header is %d bytes, MdatShortHeaderSize says %d", len(got)-4, MdatShortHeaderSize)
	}
}
