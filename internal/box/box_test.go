// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"
)

// mustHex decodes a hex string (whitespace-free) into bytes, failing the test
// on a malformed literal.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex literal %q: %v", s, err)
	}
	return b
}

func TestFourCC(t *testing.T) {
	f := NewFourCC("mp4a")
	if got := f.String(); got != "mp4a" {
		t.Fatalf("String() = %q, want %q", got, "mp4a")
	}
	if f != [4]byte{'m', 'p', '4', 'a'} {
		t.Fatalf("bytes = %v, want mp4a", f)
	}
	// A four-character code with a trailing space round-trips verbatim.
	if NewFourCC("url ").String() != "url " {
		t.Fatal("url  code did not round-trip")
	}
}

func TestAppendDescriptorSize(t *testing.T) {
	tests := []struct {
		size uint32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x81, 0x00}},
		{129, []byte{0x81, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x81, 0x80, 0x00}},
		{2097151, []byte{0xff, 0xff, 0x7f}},
		{2097152, []byte{0x81, 0x80, 0x80, 0x00}},
	}
	for _, tc := range tests {
		got := AppendDescriptorSize(nil, tc.size)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("AppendDescriptorSize(%d) = % x, want % x", tc.size, got, tc.want)
		}
	}
}

func TestAppendFtyp(t *testing.T) {
	got := AppendFtyp(nil, NewFourCC("M4A "), 0,
		NewFourCC("M4A "), NewFourCC("mp42"), NewFourCC("isom"))
	want := mustHex(t, ""+
		"0000001c"+ // size = 28
		"66747970"+ // "ftyp"
		"4d344120"+ // major_brand "M4A "
		"00000000"+ // minor_version 0
		"4d344120"+ // "M4A "
		"6d703432"+ // "mp42"
		"69736f6d") // "isom"
	if !bytes.Equal(got, want) {
		t.Fatalf("ftyp\n got % x\nwant % x", got, want)
	}
}

// TestAppendMp4aEsds cross-checks the full mp4a AudioSampleEntry, including its
// esds child, against a hand-computed reference: 48 kHz mono, ASC {0x11,0x88}.
func TestAppendMp4aEsds(t *testing.T) {
	got := AppendMp4a(nil, 1, 48000, []byte{0x11, 0x88})
	want := mustHex(t, ""+
		"0000004b"+ // mp4a box size = 75
		"6d703461"+ // "mp4a"
		"000000000000"+ // reserved[6]
		"0001"+ // data_reference_index = 1
		"00000000"+ // reserved[0]
		"00000000"+ // reserved[1]
		"0001"+ // channelcount = 1
		"0010"+ // samplesize = 16
		"0000"+ // pre_defined
		"0000"+ // reserved
		"bb800000"+ // samplerate = 48000 << 16
		// esds child
		"00000027"+ // esds size = 39
		"65736473"+ // "esds"
		"00000000"+ // version/flags
		"0319"+ // ES_Descriptor tag 0x03, len 25
		"0000"+ // ES_ID = 0
		"00"+ // flags = 0
		"0411"+ // DecoderConfigDescriptor tag 0x04, len 17
		"40"+ // objectTypeIndication = AAC
		"15"+ // streamType audio | reserved
		"000000"+ // bufferSizeDB
		"00000000"+ // maxBitrate
		"00000000"+ // avgBitrate
		"0502"+ // DecoderSpecificInfo tag 0x05, len 2
		"1188"+ // ASC
		"0601"+ // SLConfigDescriptor tag 0x06, len 1
		"02") // predefined = MP4
	if !bytes.Equal(got, want) {
		t.Fatalf("mp4a+esds\n got % x\nwant % x", got, want)
	}
}

// TestAppendEsdsSizeAt44100 exercises the 44100 Hz samplerate encoding through
// stsd and confirms the samplerate field is SampleRate << 16.
func TestSampleRate44100(t *testing.T) {
	b := AppendMp4a(nil, 2, 44100, []byte{0x12, 0x10})
	// samplerate is the last 4 bytes of the 28-byte fixed area, i.e. bytes
	// [8+24 : 8+28] = [32:36].
	got := binary.BigEndian.Uint32(b[32:36])
	if want := uint32(44100) << 16; got != want {
		t.Fatalf("samplerate = %#08x, want %#08x", got, want)
	}
}

func TestAppendStsz(t *testing.T) {
	got := AppendStsz(nil, []uint32{100, 200, 300})
	want := mustHex(t, ""+
		"00000020"+ // size = 32
		"7374737a"+ // "stsz"
		"00000000"+ // version/flags
		"00000000"+ // sample_size = 0
		"00000003"+ // sample_count = 3
		"00000064"+ // 100
		"000000c8"+ // 200
		"0000012c") // 300
	if !bytes.Equal(got, want) {
		t.Fatalf("stsz\n got % x\nwant % x", got, want)
	}
}

func TestAppendStco(t *testing.T) {
	got := AppendStco(nil, []uint32{0x11223344})
	want := mustHex(t, ""+
		"00000014"+ // size = 20
		"7374636f"+ // "stco"
		"00000000"+ // version/flags
		"00000001"+ // entry_count = 1
		"11223344") // chunk_offset
	if !bytes.Equal(got, want) {
		t.Fatalf("stco\n got % x\nwant % x", got, want)
	}
}

func TestAppendCo64(t *testing.T) {
	got := AppendCo64(nil, []uint64{0x0000000123456789})
	want := mustHex(t, ""+
		"00000018"+ // size = 24
		"636f3634"+ // "co64"
		"00000000"+ // version/flags
		"00000001"+ // entry_count = 1
		"0000000123456789") // chunk_offset
	if !bytes.Equal(got, want) {
		t.Fatalf("co64\n got % x\nwant % x", got, want)
	}
}

func TestAppendElstV0(t *testing.T) {
	got := AppendElst(nil, 48000, 1024)
	want := mustHex(t, ""+
		"0000001c"+ // size = 28
		"656c7374"+ // "elst"
		"00000000"+ // version 0 / flags
		"00000001"+ // entry_count = 1
		"0000bb80"+ // segment_duration = 48000
		"00000400"+ // media_time = 1024
		"0001"+ // media_rate_integer = 1
		"0000") // media_rate_fraction = 0
	if !bytes.Equal(got, want) {
		t.Fatalf("elst v0\n got % x\nwant % x", got, want)
	}
}

func TestAppendElstV1(t *testing.T) {
	// segment_duration 0x1_0000_0000 forces version 1 (needs > 31 bits).
	got := AppendElst(nil, 0x100000000, 1024)
	want := mustHex(t, ""+
		"00000024"+ // size = 36
		"656c7374"+ // "elst"
		"01000000"+ // version 1 / flags
		"00000001"+ // entry_count = 1
		"0000000100000000"+ // segment_duration = 2^32
		"0000000000000400"+ // media_time = 1024
		"0001"+ // media_rate_integer = 1
		"0000") // media_rate_fraction = 0
	if !bytes.Equal(got, want) {
		t.Fatalf("elst v1\n got % x\nwant % x", got, want)
	}
}

func TestAppendMdatHeader(t *testing.T) {
	got := AppendMdatHeader(nil)
	want := mustHex(t, ""+
		"00000001"+ // size == 1 => largesize form
		"6d646174"+ // "mdat"
		"0000000000000000") // placeholder largesize
	if !bytes.Equal(got, want) {
		t.Fatalf("mdat header\n got % x\nwant % x", got, want)
	}
	if len(got) != MdatHeaderSize {
		t.Fatalf("mdat header length = %d, want %d", len(got), MdatHeaderSize)
	}
	// The writer patches the largesize at MdatLargeSizeOffset.
	patched := append([]byte(nil), got...)
	ls := AppendMdatLargeSize(nil, MdatHeaderSize+4242)
	copy(patched[MdatLargeSizeOffset:], ls)
	if v := binary.BigEndian.Uint64(patched[MdatLargeSizeOffset:]); v != MdatHeaderSize+4242 {
		t.Fatalf("patched largesize = %d, want %d", v, MdatHeaderSize+4242)
	}
}

// boxSize reads the declared box length: the 32-bit size field, or the 64-bit
// largesize when the size field is 1.
func boxSize(t *testing.T, b []byte) uint64 {
	t.Helper()
	if len(b) < 8 {
		t.Fatalf("box too short: %d bytes", len(b))
	}
	size := binary.BigEndian.Uint32(b)
	if size == 1 {
		if len(b) < 16 {
			t.Fatalf("largesize box too short: %d bytes", len(b))
		}
		return binary.BigEndian.Uint64(b[8:16])
	}
	return uint64(size)
}

// TestDeclaredSizeMatchesLength verifies every marshaled box declares a size
// equal to the number of bytes it wrote. The mdat header is exempt because its
// largesize is a Close-time placeholder, not the header length.
func TestDeclaredSizeMatchesLength(t *testing.T) {
	asc := []byte{0x11, 0x88}
	// A representative child payload for the container assemblers.
	stbl := AppendStbl(nil, func() []byte {
		var c []byte
		c = AppendStsd(c, 1, 48000, asc)
		c = AppendStts(c, 3, 1024)
		c = AppendStsc(c, 1, 3, 1)
		c = AppendStsz(c, []uint32{100, 200, 300})
		c = AppendStco(c, []uint32{40})
		return c
	}())

	boxes := map[string][]byte{
		"ftyp":    AppendFtyp(nil, NewFourCC("M4A "), 0, NewFourCC("M4A "), NewFourCC("mp42"), NewFourCC("isom")),
		"mvhd_v0": AppendMvhd(nil, 48000, 48000),
		"mvhd_v1": AppendMvhd(nil, 48000, math.MaxUint32+1),
		"tkhd_v0": AppendTkhd(nil, 1, 48000),
		"tkhd_v1": AppendTkhd(nil, 1, math.MaxUint32+1),
		"mdhd_v0": AppendMdhd(nil, 48000, 48000),
		"mdhd_v1": AppendMdhd(nil, 48000, math.MaxUint32+1),
		"elst_v0": AppendElst(nil, 48000, 1024),
		"elst_v1": AppendElst(nil, 0x100000000, 1024),
		"hdlr":    AppendHdlr(nil, NewFourCC("soun"), "SoundHandler"),
		"smhd":    AppendSmhd(nil),
		"url":     AppendURLEntry(nil),
		"dref":    AppendDref(nil),
		"dinf":    AppendDinf(nil),
		"esds":    AppendEsds(nil, asc),
		"mp4a":    AppendMp4a(nil, 1, 48000, asc),
		"stsd":    AppendStsd(nil, 1, 48000, asc),
		"stts":    AppendStts(nil, 3, 1024),
		"stsc":    AppendStsc(nil, 1, 3, 1),
		"stsz":    AppendStsz(nil, []uint32{100, 200, 300}),
		"stco":    AppendStco(nil, []uint32{40}),
		"co64":    AppendCo64(nil, []uint64{40}),
		"stbl":    stbl,
		"minf":    AppendMinf(nil, AppendSmhd(nil)),
		"mdia":    AppendMdia(nil, AppendSmhd(nil)),
		"edts":    AppendEdts(nil, AppendElst(nil, 48000, 1024)),
		"trak":    AppendTrak(nil, AppendTkhd(nil, 1, 48000)),
		"moov":    AppendMoov(nil, AppendMvhd(nil, 48000, 48000)),
	}
	for name, b := range boxes {
		if got, want := boxSize(t, b), uint64(len(b)); got != want {
			t.Errorf("%s: declared size %d != actual length %d", name, got, want)
		}
	}
}

// TestVersionSelectionBoundary proves the version-0/version-1 flip happens at
// the documented boundary for each version-selecting box.
func TestVersionSelectionBoundary(t *testing.T) {
	// version byte is the first byte after the 8-byte box header.
	versionOf := func(b []byte) byte { return b[8] }

	// mvhd/tkhd/mdhd switch when duration exceeds 32 bits (MaxUint32).
	for _, tc := range []struct {
		name string
		fn   func(duration uint64) []byte
	}{
		{"mvhd", func(d uint64) []byte { return AppendMvhd(nil, 48000, d) }},
		{"tkhd", func(d uint64) []byte { return AppendTkhd(nil, 1, d) }},
		{"mdhd", func(d uint64) []byte { return AppendMdhd(nil, 48000, d) }},
	} {
		if v := versionOf(tc.fn(math.MaxUint32)); v != 0 {
			t.Errorf("%s duration=MaxUint32: version %d, want 0", tc.name, v)
		}
		if v := versionOf(tc.fn(math.MaxUint32 + 1)); v != 1 {
			t.Errorf("%s duration=MaxUint32+1: version %d, want 1", tc.name, v)
		}
	}

	// elst switches when segment_duration needs > 31 bits (it is
	// signed-effective at v0), i.e. at MaxInt32 -> v0, MaxInt32+1 -> v1.
	if v := versionOf(AppendElst(nil, math.MaxInt32, 0)); v != 0 {
		t.Errorf("elst seg=MaxInt32: version %d, want 0", v)
	}
	if v := versionOf(AppendElst(nil, math.MaxInt32+1, 0)); v != 1 {
		t.Errorf("elst seg=MaxInt32+1: version %d, want 1", v)
	}
	// A media_time outside int32 range also forces v1.
	if v := versionOf(AppendElst(nil, 0, math.MaxInt32+1)); v != 1 {
		t.Errorf("elst media_time=MaxInt32+1: version %d, want 1", v)
	}
	if v := versionOf(AppendElst(nil, 0, math.MinInt32-1)); v != 1 {
		t.Errorf("elst media_time=MinInt32-1: version %d, want 1", v)
	}
	if v := versionOf(AppendElst(nil, 0, math.MinInt32)); v != 0 {
		t.Errorf("elst media_time=MinInt32: version %d, want 0", v)
	}
}

// TestAppendZeroAllocFriendly confirms the append idiom reuses caller capacity:
// writing into a pre-sized buffer performs no growth.
func TestAppendReusesCapacity(t *testing.T) {
	buf := make([]byte, 0, 512)
	ptr := &buf[:1][0]
	buf = AppendFtyp(buf, NewFourCC("M4A "), 0, NewFourCC("isom"))
	buf = AppendMvhd(buf, 48000, 48000)
	if &buf[:1][0] != ptr {
		t.Fatal("append reallocated despite sufficient capacity")
	}
}
