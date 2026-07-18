// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// walkNoPanic runs WalkChildren over payload with a no-op visitor, recovering any
// panic into a fatal failure, and returns the error. It is the box-level analogue
// of the reader's noPanic helpers: a malformed child table must be rejected, never
// crash.
func walkNoPanic(t *testing.T, payload []byte) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic in WalkChildren on malformed payload: %v", p)
		}
	}()
	return WalkChildren(payload, func(_ FourCC, _ []byte) error { return nil })
}

// TestWalkChildrenLargesizeTruncation locks in the WalkChildren int64 fix. The
// child header is the 64-bit largesize form (size==1) with a largesize whose low
// 32 bits are small (0x0000000200000008). The pre-fix code narrowed the size to
// int before the bound check, so on a 32-bit build the high word was truncated
// away and the surviving low word (8, smaller than the 16-byte header) drove a
// payload[16:8] slice out of range. On any word size the total must be treated as
// int64 and the child rejected as running past the parent, wrapping errParse,
// with no panic.
func TestWalkChildrenLargesizeTruncation(t *testing.T) {
	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, 1) // size==1 => largesize form
	payload = append(payload, 'f', 'r', 'e', 'e')       // child type
	payload = binary.BigEndian.AppendUint64(payload, 0x0000000200000008)
	// payload is exactly the 16-byte header; the declared largesize dwarfs it.

	err := walkNoPanic(t, payload)
	if err == nil {
		t.Fatalf("WalkChildren accepted a largesize %#x past the parent", uint64(0x0000000200000008))
	}
	if !errors.Is(err, errParse) {
		t.Fatalf("error = %v, want wrapped errParse", err)
	}
}

// TestWalkChildrenOverLengthChild covers the plain 32-bit size form: a child that
// declares more bytes than the payload holds must be rejected as running past the
// parent, wrapping errParse, without panic.
func TestWalkChildrenOverLengthChild(t *testing.T) {
	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, 64)      // claims 64 bytes...
	payload = append(payload, 'f', 'r', 'e', 'e', 0, 0, 0, 0) // ...but only 12 bytes are present

	err := walkNoPanic(t, payload)
	if err == nil {
		t.Fatal("WalkChildren accepted a child longer than the payload")
	}
	if !errors.Is(err, errParse) {
		t.Fatalf("error = %v, want wrapped errParse", err)
	}
}

// mp4aBodyVersion assembles an mp4a AudioSampleEntry body (the bytes after the
// 8-byte box header) with a chosen QuickTime sound-description version, so
// ParseAudioSampleEntry exercises the version 1 (+16) and version 2 (+36)
// child-offset adjustments. It writes the fixed 28-byte AudioSampleEntry, then
// extra bytes of QuickTime padding, then a real esds child so the returned
// childOffset can be shown to land exactly on it.
func mp4aBodyVersion(version, channels uint16, sampleRate uint32, extra int, asc []byte) []byte {
	body := make([]byte, 28)
	// body[0:6] reserved, body[6:8] data_reference_index.
	binary.BigEndian.PutUint16(body[6:], 1)
	// body[8:10] is the QuickTime sound-description version ParseAudioSampleEntry
	// reads; the rest of reserved[0]/reserved[1] stays zero.
	binary.BigEndian.PutUint16(body[8:], version)
	binary.BigEndian.PutUint16(body[16:], channels)
	binary.BigEndian.PutUint16(body[18:], 16) // samplesize
	binary.BigEndian.PutUint32(body[24:], sampleRate<<16)
	body = append(body, make([]byte, extra)...)
	return append(body, AppendEsds(nil, asc)...)
}

// TestParseAudioSampleEntryQuickTimeVersions covers the QuickTime version 1 and
// version 2 sound sample entries, whose extra fixed fields shift where the child
// boxes begin. ParseAudioSampleEntry must still report the right channelcount and
// samplerate and return a childOffset that lands on the following esds.
func TestParseAudioSampleEntryQuickTimeVersions(t *testing.T) {
	asc := []byte{0x12, 0x10}
	tests := []struct {
		name          string
		version       uint16
		extra         int
		wantChildOff  int
		wantChannels  uint16
		wantSampleRat uint32
	}{
		{"version1", 1, 16, 28 + 16, 2, 44100},
		{"version2", 2, 36, 28 + 36, 2, 44100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := mp4aBodyVersion(tc.version, tc.wantChannels, tc.wantSampleRat, tc.extra, asc)
			ch, rate, childOff, err := ParseAudioSampleEntry(body)
			if err != nil {
				t.Fatalf("ParseAudioSampleEntry: %v", err)
			}
			if ch != tc.wantChannels {
				t.Errorf("channels = %d, want %d", ch, tc.wantChannels)
			}
			if rate != tc.wantSampleRat {
				t.Errorf("sampleRate = %d, want %d", rate, tc.wantSampleRat)
			}
			if childOff != tc.wantChildOff {
				t.Errorf("childOffset = %d, want %d", childOff, tc.wantChildOff)
			}

			// The esds child must be locatable at childOff.
			var foundASC []byte
			err = WalkChildren(body[childOff:], func(typ FourCC, b []byte) error {
				if typ != NewFourCC("esds") {
					return nil
				}
				a, _, perr := ParseEsds(b)
				foundASC = a
				return perr
			})
			if err != nil {
				t.Fatalf("walk esds at childOffset %d: %v", childOff, err)
			}
			if !bytes.Equal(foundASC, asc) {
				t.Errorf("esds ASC at childOffset = % x, want % x", foundASC, asc)
			}
		})
	}
}

// TestParseHeaderLargesizeTooLarge confirms a 64-bit largesize above 2^62 is
// rejected rather than allowed to wrap the signed int64 conversion. The value
// (1<<62)+1 is past the guard but well inside uint64.
func TestParseHeaderLargesizeTooLarge(t *testing.T) {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, 1) // size==1 => largesize form
	b = append(b, 'm', 'd', 'a', 't')
	b = binary.BigEndian.AppendUint64(b, (uint64(1)<<62)+1)

	if _, err := ParseHeader(b); err == nil {
		t.Fatal("ParseHeader accepted a largesize above 2^62")
	} else if !errors.Is(err, errParse) {
		t.Fatalf("error = %v, want wrapped errParse", err)
	}
}

// TestParseEsdsURLFlag drives the ES_Descriptor URL_Flag branch: an esds whose
// ES_Descriptor sets URL_Flag (0x40) carries a URLlength and URLstring before the
// DecoderConfigDescriptor. ParseEsds must skip the URL field and still recover the
// ASC and object type, without panic.
func TestParseEsdsURLFlag(t *testing.T) {
	asc := []byte{0x11, 0x88}

	dsi := appendDescriptor(nil, tagDecoderSpecificInfo, asc)
	var dcd []byte
	dcd = append(dcd, objectTypeAAC, streamTypeAudio)
	dcd = appendU24(dcd, 0) // bufferSizeDB
	dcd = appendU32(dcd, 0) // maxBitrate
	dcd = appendU32(dcd, 0) // avgBitrate
	dcd = append(dcd, dsi...)
	dcdDesc := appendDescriptor(nil, tagDecoderConfig, dcd)
	sl := appendDescriptor(nil, tagSLConfig, []byte{slPredefinedMP4})

	const url = "u" // a one-character URLstring
	var es []byte
	es = appendU16(es, 0)                 // ES_ID
	es = append(es, 0x40, byte(len(url))) // flags: URL_Flag set, then URLlength
	es = append(es, url...)               // URLstring
	es = append(es, dcdDesc...)
	es = append(es, sl...)
	esDesc := appendDescriptor(nil, tagESDescriptor, es)

	body := make([]byte, 4, 4+len(esDesc)) // esds FullBox version/flags
	body = append(body, esDesc...)

	gotASC, objType, err := func() (a []byte, ot byte, err error) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic in ParseEsds on URL_Flag descriptor: %v", p)
			}
		}()
		return ParseEsds(body)
	}()
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
