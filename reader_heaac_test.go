// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"testing"
)

// Complete AudioSpecificConfigs for the HE-AAC labeling test. Each is a whole
// explicit-hierarchical config that go-aac parses without overreading a truncated
// buffer, so the test exercises a valid HE-AAC signal rather than a two-byte stub:
//
//	ascAACLC3:  audioObjectType 2 (AAC-LC), samplingFrequencyIndex 3 (48 kHz),
//	            channelConfiguration 1, then a zero GASpecificConfig padding byte.
//	ascHEAAC:   audioObjectType 5 (SBR), sfi 3, chanCfg 1, extensionSFI 3, base AOT 2.
//	ascHEAACv2: audioObjectType 29 (PS), sfi 3, chanCfg 1, extensionSFI 3, base AOT 2.
//
// The writer accepts only an AAC-LC object type, so the fixtures are built with
// ascAACLC3 and then rewritten in place to an HE-AAC config; keeping the length
// equal leaves the esds descriptor size and every enclosing box size untouched.
var (
	ascAACLC3  = []byte{0x11, 0x88, 0x00}
	ascHEAAC   = []byte{0x29, 0x89, 0x88}
	ascHEAACv2 = []byte{0xe9, 0x89, 0x88}
)

// rewriteASC returns a copy of an AAC .m4a with its AudioSpecificConfig replaced by
// want, which must be the same length as have. It locates the ASC through the esds
// DecoderSpecificInfo descriptor (tag 0x05, minimal size byte, then the ASC bytes)
// that the writer emits, and fails the test if that descriptor is not present
// exactly once.
func rewriteASC(t *testing.T, data, have, want []byte) []byte {
	t.Helper()
	if len(have) != len(want) {
		t.Fatalf("rewriteASC needs equal lengths, have %d want %d", len(have), len(want))
	}
	needle := append([]byte{0x05, byte(len(have))}, have...)
	if n := bytes.Count(data, needle); n != 1 {
		t.Fatalf("esds DecoderSpecificInfo needle % x appears %d times, want 1", needle, n)
	}
	out := append([]byte(nil), data...)
	copy(out[bytes.Index(data, needle)+2:], want)
	return out
}

// TestReaderLabelsHEAACAsAACLC pins the deliberate choice that the container reader
// identifies the MPEG-4 Audio family (esds objectTypeIndication) and does not
// verify the AAC profile: an HE-AAC (audio object type 5) or HE-AACv2 (29) track
// still reads back as CodecAACLC, with rejection left to the decoder. Making the
// reader reject HE-AAC would turn this test red on purpose, which is the point:
// that change must be a conscious edit here, not silent.
func TestReaderLabelsHEAACAsAACLC(t *testing.T) {
	base := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascAACLC3}, synthFrames(4))

	for _, tc := range []struct {
		name string
		asc  []byte
	}{
		{"HE-AAC (SBR, AOT 5)", ascHEAAC},
		{"HE-AACv2 (PS, AOT 29)", ascHEAACv2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := rewriteASC(t, base, ascAACLC3, tc.asc)
			rd, err := NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewReader on %s = %v, want it accepted as a container", tc.name, err)
			}
			if got := rd.Info().Codec; got != CodecAACLC {
				t.Fatalf("Info.Codec = %v, want CodecAACLC (reader is profile-agnostic)", got)
			}
		})
	}
}
