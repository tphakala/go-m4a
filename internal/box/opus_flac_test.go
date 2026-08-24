// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestAppendDopsMatchesFFmpeg(t *testing.T) {
	// The exact dOps box ffmpeg emits for a 48 kHz mono Opus stream: header, then
	// Version 0, OutputChannelCount 1, PreSkip 312 (0x0138), InputSampleRate 48000
	// (0x0000bb80), OutputGain 0, ChannelMappingFamily 0.
	want, _ := hex.DecodeString("00000013644f707300010138" + "0000bb80" + "000000")
	got := AppendDops(nil, 1, 312, 48000)
	if !bytes.Equal(got, want) {
		t.Errorf("AppendDops = %x, want %x", got, want)
	}
	// Round-trip through the parser.
	ch, preSkip, rate, err := ParseDops(got[8:])
	if err != nil {
		t.Fatalf("ParseDops: %v", err)
	}
	if ch != 1 || preSkip != 312 || rate != 48000 {
		t.Errorf("ParseDops = (ch %d, preSkip %d, rate %d), want (1, 312, 48000)", ch, preSkip, rate)
	}
}

// TestParseDopsMalformed feeds ParseDops hostile bytes: it must return an error,
// never panic, since the reader hands it untrusted input.
func TestParseDopsMalformed(t *testing.T) {
	badVersion := make([]byte, 11)
	badVersion[0] = 1 // version != 0
	cases := map[string][]byte{
		"empty":       {},
		"one short":   make([]byte, 10), // one under the 11-byte minimum
		"bad version": badVersion,
	}
	for name, in := range cases {
		if _, _, _, err := ParseDops(in); err == nil {
			t.Errorf("%s (%d bytes): ParseDops returned nil error, want a parse error", name, len(in))
		}
	}
}

// TestParseDflaMalformed feeds ParseDfla hostile bytes: it must return an error,
// never panic (in particular the 24-bit length must be bounds-checked).
func TestParseDflaMalformed(t *testing.T) {
	// A well-formed dfLa header whose STREAMINFO block claims 20 bytes (not the
	// fixed 34) with 20 bytes present: fits the buffer but is the wrong length.
	wrongLen := append([]byte{0, 0, 0, 0, 0x80, 0, 0, 20}, make([]byte, 20)...)
	cases := map[string][]byte{
		"empty":           {},
		"too short":       make([]byte, 7),                      // under version/flags(4)+block header(4)
		"not streaminfo":  {0, 0, 0, 0, 0x81, 0, 0, 34},         // block type 1, not STREAMINFO(0)
		"length overruns": {0, 0, 0, 0, 0x80, 0, 0, 34},         // claims 34 bytes, zero present
		"huge length":     {0, 0, 0, 0, 0x80, 0xff, 0xff, 0xff}, // 24-bit length far past the buffer
		"wrong length":    wrongLen,                             // 20 bytes present but STREAMINFO is fixed 34
	}
	for name, in := range cases {
		if _, err := ParseDfla(in); err == nil {
			t.Errorf("%s (%d bytes): ParseDfla returned nil error, want a parse error", name, len(in))
		}
	}
}

func TestAppendDflaRoundTrip(t *testing.T) {
	// A synthetic 34-byte STREAMINFO body (contents opaque to the container).
	si := make([]byte, StreamInfoBodyLen)
	for i := range si {
		si[i] = byte(i + 1)
	}
	dfla := AppendDfla(nil, si)

	// The dfLa body starts after the 8-byte box header.
	got, err := ParseDfla(dfla[8:])
	if err != nil {
		t.Fatalf("ParseDfla: %v", err)
	}
	if !bytes.Equal(got, si) {
		t.Errorf("ParseDfla returned %x, want %x", got, si)
	}
	// FullBox version/flags (4 zero bytes) then last-block|type-0 header (0x80).
	body := dfla[8:]
	if body[4] != 0x80 {
		t.Errorf("dfLa block header = %#x, want 0x80 (last | STREAMINFO)", body[4])
	}
}

// StreamInfoBodyLen mirrors go-flac's meta.StreamInfoBodyLen for the fixture size.
const StreamInfoBodyLen = 34

func TestAppendSttsRuns(t *testing.T) {
	// A two-run table like Opus/FLAC produce: 50 samples of 960, then 1 of 312.
	runs := []SttsRun{{Count: 50, Delta: 960}, {Count: 1, Delta: 312}}
	stts := AppendSttsRuns(nil, runs)

	samples, dur, err := ParseStts(stts[8:]) // body after the 8-byte box header
	if err != nil {
		t.Fatalf("ParseStts: %v", err)
	}
	if samples != 51 {
		t.Errorf("total samples = %d, want 51", samples)
	}
	if want := uint64(50*960 + 312); dur != want {
		t.Errorf("total duration = %d, want %d", dur, want)
	}
	// AppendStts must be the single-run special case.
	if !bytes.Equal(AppendStts(nil, 51, 1024), AppendSttsRuns(nil, []SttsRun{{Count: 51, Delta: 1024}})) {
		t.Error("AppendStts differs from the equivalent single-run AppendSttsRuns")
	}
}

func TestOpusFlacSampleEntriesParse(t *testing.T) {
	// An Opus sample entry (dOps child) must parse via the shared AudioSampleEntry
	// reader, with the child box reachable at childOffset.
	dops := AppendDops(nil, 2, 312, 48000)
	opus := AppendOpusEntry(nil, 2, 48000, dops)
	assertSampleEntry(t, opus, fourCCOpus, 2, 48000, fourCCDops)

	si := make([]byte, StreamInfoBodyLen)
	dfla := AppendDfla(nil, si)
	flac := AppendFlacEntry(nil, 1, 44100, dfla)
	assertSampleEntry(t, flac, fourCCFlac, 1, 44100, fourCCDfla)
}

// TestFLACSampleEntryRateReduction pins the power-of-two reduction AppendFlacEntry
// applies so a rate above the 16.16 field ceiling does not overflow it. The
// standard high rates land on a real base rate, matching the Xiph FLAC-in-ISOBMFF
// spec and ffmpeg; the true rate is carried by STREAMINFO and the container
// timescale, not this legacy hint.
func TestFLACSampleEntryRateReduction(t *testing.T) {
	cases := []struct {
		rate uint32
		want uint32
	}{
		{8000, 8000},
		{44100, 44100},
		{48000, 48000},
		{65535, 65535},
		{65536, 32768},
		{88200, 44100},
		{96000, 48000},
		{176400, 44100},
		{192000, 48000},
		{1048575, 65535}, // the 20-bit STREAMINFO maximum halves down to the field ceiling
	}
	for _, tc := range cases {
		if got := flacSampleEntryRate(tc.rate); got != tc.want {
			t.Errorf("flacSampleEntryRate(%d) = %d, want %d", tc.rate, got, tc.want)
		}
		// The value actually written into the fLaC AudioSampleEntry is the reduced one.
		si := make([]byte, StreamInfoBodyLen)
		flac := AppendFlacEntry(nil, 2, tc.rate, AppendDfla(nil, si))
		assertSampleEntry(t, flac, fourCCFlac, 2, tc.want, fourCCDfla)
	}
}

// TestSTREAMINFOSampleRate pins the 20-bit rate extraction used on the read path to
// recover a FLAC track's true rate from STREAMINFO rather than the reduced sample
// entry.
func TestSTREAMINFOSampleRate(t *testing.T) {
	for _, rate := range []uint32{8000, 44100, 96000, 192000, 0xFFFFF} {
		si := make([]byte, StreamInfoBodyLen)
		// Pack the 20-bit rate starting at byte 10, per RFC 9639 section 8.2.
		si[10] = byte(rate >> 12)
		si[11] = byte(rate >> 4)
		si[12] = byte(rate << 4)
		if got := STREAMINFOSampleRate(si); got != rate {
			t.Errorf("STREAMINFOSampleRate(%d packed) = %d, want %d", rate, got, rate)
		}
	}
	// A block too short to hold the field yields 0, so a reader falls back to the
	// sample entry rather than reading past the slice.
	if got := STREAMINFOSampleRate(make([]byte, 12)); got != 0 {
		t.Errorf("STREAMINFOSampleRate(short) = %d, want 0", got)
	}
}

func assertSampleEntry(t *testing.T, entry []byte, wantType FourCC, wantCh uint16, wantRate uint32, wantChild FourCC) {
	t.Helper()
	h, err := ParseHeader(entry)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Type != wantType {
		t.Errorf("sample entry type = %q, want %q", h.Type, wantType)
	}
	body := entry[h.HeaderLen:h.Total]
	ch, rate, childOff, err := ParseAudioSampleEntry(body)
	if err != nil {
		t.Fatalf("ParseAudioSampleEntry: %v", err)
	}
	if ch != wantCh || rate != wantRate {
		t.Errorf("AudioSampleEntry = (ch %d, rate %d), want (%d, %d)", ch, rate, wantCh, wantRate)
	}
	child, err := ParseHeader(body[childOff:])
	if err != nil {
		t.Fatalf("child ParseHeader: %v", err)
	}
	if child.Type != wantChild {
		t.Errorf("child box = %q, want %q", child.Type, wantChild)
	}
}
