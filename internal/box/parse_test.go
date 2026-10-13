// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

// bodyOf parses full as a single box and returns its body (the bytes after the
// header), the bridge between the Append* marshalers (which emit whole boxes)
// and the Parse* functions (which take box bodies).
func bodyOf(t *testing.T, full []byte) []byte {
	t.Helper()
	h, err := ParseHeader(full)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.ToEnd || h.Total > int64(len(full)) {
		t.Fatalf("box total %d exceeds %d bytes", h.Total, len(full))
	}
	return full[h.HeaderLen:h.Total]
}

func TestParseHeaderForms(t *testing.T) {
	// 32-bit form.
	h, err := ParseHeader(AppendBoxHeader(nil, 24, NewFourCC("moov")))
	if err != nil || h.HeaderLen != 8 || h.Total != 24 || h.Type != NewFourCC("moov") {
		t.Fatalf("32-bit header: %+v err=%v", h, err)
	}
	// 64-bit largesize form.
	h, err = ParseHeader(AppendLargeBoxHeader(nil, 1<<33, NewFourCC("mdat")))
	if err != nil || h.HeaderLen != 16 || h.Total != 1<<33 {
		t.Fatalf("largesize header: %+v err=%v", h, err)
	}
	// size==0 extends to EOF.
	h, err = ParseHeader([]byte{0, 0, 0, 0, 'm', 'd', 'a', 't'})
	if err != nil || !h.ToEnd || h.HeaderLen != 8 {
		t.Fatalf("size-0 header: %+v err=%v", h, err)
	}
}

func TestParseHeaderRejects(t *testing.T) {
	cases := map[string][]byte{
		"truncated":       {0, 0, 0},
		"size below 8":    {0, 0, 0, 7, 'f', 'r', 'e', 'e'},
		"largesize short": {0, 0, 0, 1, 'm', 'd', 'a', 't'},
		"largesize below 16": append([]byte{0, 0, 0, 1, 'm', 'd', 'a', 't'},
			0, 0, 0, 0, 0, 0, 0, 8),
	}
	for name, b := range cases {
		if _, err := ParseHeader(b); err == nil {
			t.Errorf("%s: ParseHeader succeeded, want error", name)
		}
	}
}

func TestReadDescriptorSize(t *testing.T) {
	tests := []struct {
		name   string
		in     []byte
		want   uint32
		wantN  int
		wantOK bool
	}{
		{"one byte", []byte{0x25}, 0x25, 1, true},
		{"two bytes", []byte{0x81, 0x00}, 128, 2, true},
		{"ffmpeg four-byte padded", []byte{0x80, 0x80, 0x80, 0x25}, 0x25, 4, true},
		{"max four continuation", []byte{0x80, 0x80, 0x80, 0x80, 0x25}, 0x25, 5, true},
		{"five continuation rejected", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x25}, 0, 0, false},
		{"truncated", []byte{0x80}, 0, 0, false},
	}
	for _, tc := range tests {
		got, n, ok := readDescriptorSize(tc.in)
		if got != tc.want || n != tc.wantN || ok != tc.wantOK {
			t.Errorf("%s: readDescriptorSize = (%d, %d, %v), want (%d, %d, %v)",
				tc.name, got, n, ok, tc.want, tc.wantN, tc.wantOK)
		}
	}
}

func TestParseEsdsRoundTrip(t *testing.T) {
	asc := []byte{0x11, 0x88, 0x56, 0xe5, 0x00} // a longer-than-2-byte ASC, as ffmpeg emits
	full := AppendEsds(nil, asc)
	gotASC, objType, err := ParseEsds(bodyOf(t, full))
	if err != nil {
		t.Fatalf("ParseEsds: %v", err)
	}
	if !bytes.Equal(gotASC, asc) {
		t.Errorf("ASC = % x, want % x", gotASC, asc)
	}
	if objType != objectTypeAAC {
		t.Errorf("objectType = %#x, want %#x", objType, objectTypeAAC)
	}
}

func TestParseAudioSampleEntryRoundTrip(t *testing.T) {
	asc := []byte{0x12, 0x10}
	full := AppendMp4a(nil, 2, 44100, asc)
	body := bodyOf(t, full)
	ch, rate, childOff, err := ParseAudioSampleEntry(body)
	if err != nil {
		t.Fatalf("ParseAudioSampleEntry: %v", err)
	}
	if ch != 2 || rate != 44100 {
		t.Errorf("channels/rate = %d/%d, want 2/44100", ch, rate)
	}
	// The esds child must be locatable at childOff.
	var foundASC []byte
	err = WalkChildren(body[childOff:], func(typ FourCC, b []byte) error {
		if typ == NewFourCC("esds") {
			a, _, perr := ParseEsds(b)
			foundASC = a
			return perr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk esds: %v", err)
	}
	if !bytes.Equal(foundASC, asc) {
		t.Errorf("esds ASC = % x, want % x", foundASC, asc)
	}
}

func TestParseSampleTablesRoundTrip(t *testing.T) {
	// stsz per-sample.
	if _, count, sizes, err := ParseStsz(bodyOf(t, AppendStsz(nil, []uint32{100, 200, 300}))); err != nil {
		t.Fatalf("ParseStsz: %v", err)
	} else if count != 3 || !bytes.Equal(u32s(sizes), u32s([]uint32{100, 200, 300})) {
		t.Errorf("stsz = count %d sizes %v", count, sizes)
	}

	// stsc two runs.
	full := AppendFullBoxHeader(nil, 40, NewFourCC("stsc"), 0, 0)
	full = binary.BigEndian.AppendUint32(full, 2) // entry_count
	full = binary.BigEndian.AppendUint32(full, 1) // first_chunk
	full = binary.BigEndian.AppendUint32(full, 24)
	full = binary.BigEndian.AppendUint32(full, 1)
	full = binary.BigEndian.AppendUint32(full, 2) // first_chunk
	full = binary.BigEndian.AppendUint32(full, 7)
	full = binary.BigEndian.AppendUint32(full, 1)
	entries, err := ParseStsc(bodyOf(t, full))
	if err != nil || len(entries) != 2 || entries[1].FirstChunk != 2 || entries[1].SamplesPerChunk != 7 {
		t.Fatalf("ParseStsc = %+v err=%v", entries, err)
	}

	// stco and co64.
	if offs, err := ParseStco(bodyOf(t, AppendStco(nil, []uint32{4096, 5783}))); err != nil || len(offs) != 2 || offs[1] != 5783 {
		t.Fatalf("ParseStco = %v err=%v", offs, err)
	}
	if offs, err := ParseCo64(bodyOf(t, AppendCo64(nil, []uint64{1 << 33}))); err != nil || len(offs) != 1 || offs[0] != 1<<33 {
		t.Fatalf("ParseCo64 = %v err=%v", offs, err)
	}

	// stts sums.
	if cnt, dur, err := ParseStts(bodyOf(t, AppendStts(nil, 30, 1024))); err != nil || cnt != 30 || dur != 30*1024 {
		t.Fatalf("ParseStts = %d/%d err=%v", cnt, dur, err)
	}
	if _, _, err := ParseStts([]byte{0, 0, 0, 0, 0, 0, 0, 1}); !errors.Is(err, errParse) {
		t.Fatalf("ParseStts overrun err = %v, want errParse", err)
	}
}

func TestParseElst(t *testing.T) {
	// v0Body builds a version-0 body of (segment, media_time, rate) triples.
	v0Body := func(rows ...[3]uint32) []byte {
		b := []byte{0, 0, 0, 0, 0, 0, 0, byte(len(rows))}
		for _, r := range rows {
			b = binary.BigEndian.AppendUint32(b, r[0])
			b = binary.BigEndian.AppendUint32(b, r[1])
			b = binary.BigEndian.AppendUint32(b, r[2])
		}
		return b
	}
	v1Body := func() []byte {
		b := []byte{1, 0, 0, 0, 0, 0, 0, 2}
		b = binary.BigEndian.AppendUint64(b, 1<<33)
		b = binary.BigEndian.AppendUint64(b, ^uint64(0)) // -1
		b = binary.BigEndian.AppendUint32(b, 0x00010000)
		b = binary.BigEndian.AppendUint64(b, 7)
		b = binary.BigEndian.AppendUint64(b, 1024)
		b = binary.BigEndian.AppendUint32(b, 0)
		return b
	}
	tests := []struct {
		name    string
		payload []byte
		want    []ElstEntry
		wantErr bool
	}{
		{"v0 round trip", bodyOf(t, AppendElst(nil, 600, 1024)), []ElstEntry{{600, 1024, 0x00010000}}, false},
		{"v1 round trip", bodyOf(t, AppendElst(nil, 1<<33, -1)), []ElstEntry{{1 << 33, -1, 0x00010000}}, false},
		{
			"v0 two entries", v0Body([3]uint32{500, 0xFFFFFFFF, 0x00010000}, [3]uint32{1000, 1024, 0x00010000}),
			[]ElstEntry{{500, -1, 0x00010000}, {1000, 1024, 0x00010000}},
			false,
		},
		{"v1 two entries", v1Body(), []ElstEntry{{1 << 33, -1, 0x00010000}, {7, 1024, 0}}, false},
		{"v0 negative media_time", v0Body([3]uint32{1, 0xFFFFFFFE, 0x00010000}), []ElstEntry{{1, -2, 0x00010000}}, false},
		{"count zero", []byte{0, 0, 0, 0, 0, 0, 0, 0}, nil, false},
		{"truncated count", func() []byte { b := v0Body([3]uint32{1, 2, 3}); b[7] = 3; return b }(), nil, true},
		{"count overruns body", []byte{0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0}, nil, true},
		{"under 8 bytes", []byte{0, 0, 0, 0, 0, 0, 0}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseElst(tc.payload)
			if tc.wantErr {
				if !errors.Is(err, errParse) {
					t.Fatalf("err = %v, want errParse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("entries = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseHeadersRoundTrip(t *testing.T) {
	if ts, dur, err := ParseMvhd(bodyOf(t, AppendMvhd(nil, 48000, 31744))); err != nil || ts != 48000 || dur != 31744 {
		t.Fatalf("ParseMvhd = %d/%d err=%v", ts, dur, err)
	}
	if ts, dur, err := ParseMdhd(bodyOf(t, AppendMdhd(nil, 44100, 1<<33))); err != nil || ts != 44100 || dur != 1<<33 {
		t.Fatalf("ParseMdhd = %d/%d err=%v", ts, dur, err)
	}
	if ht, err := ParseHdlr(bodyOf(t, AppendHdlr(nil, NewFourCC("soun"), "SoundHandler"))); err != nil || ht != NewFourCC("soun") {
		t.Fatalf("ParseHdlr = %q err=%v", ht, err)
	}
}

// u32s renders a uint32 slice as bytes for comparison in a single expression.
func u32s(v []uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

func TestParseSttsRunsRoundTrip(t *testing.T) {
	want := []SttsRun{{Count: 29, Delta: 1024}, {Count: 0, Delta: 5}, {Count: 1, Delta: 128}}
	got, err := ParseSttsRuns(bodyOf(t, AppendSttsRuns(nil, want)))
	if err != nil {
		t.Fatalf("ParseSttsRuns: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d runs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSttsRunsOverrun(t *testing.T) {
	body := bodyOf(t, AppendSttsRuns(nil, []SttsRun{{Count: 1, Delta: 1}}))
	binary.BigEndian.PutUint32(body[4:], 2) // claims two entries, carries one
	if _, err := ParseSttsRuns(body); !errors.Is(err, errParse) {
		t.Fatalf("overrun error = %v, want errParse", err)
	}
	if _, err := ParseSttsRuns(body[:4]); !errors.Is(err, errParse) {
		t.Fatalf("short body error = %v, want errParse", err)
	}
}
