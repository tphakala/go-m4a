// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"testing"
)

// patchASCObjectType returns a copy of an AAC .m4a with the AudioSpecificConfig
// audio object type (the top 5 bits of the first ASC byte) set to aot, locating
// the ASC through the esds DecoderSpecificInfo descriptor (tag 0x05, minimal size,
// then the ASC bytes) that the writer emits. It fails the test if the descriptor
// is not found exactly once.
func patchASCObjectType(t *testing.T, data, asc []byte, aot byte) []byte {
	t.Helper()
	needle := append([]byte{0x05, byte(len(asc))}, asc...)
	if n := bytes.Count(data, needle); n != 1 {
		t.Fatalf("esds DecoderSpecificInfo needle % x appears %d times, want 1", needle, n)
	}
	ascStart := bytes.Index(data, needle) + 2
	out := append([]byte(nil), data...)
	out[ascStart] = (out[ascStart] & 0x07) | (aot << 3)
	return out
}

// TestReaderLabelsHEAACAsAACLC pins the deliberate choice that the container reader
// identifies the MPEG-4 Audio family (esds objectTypeIndication) and does not
// verify the AAC profile: an HE-AAC (audio object type 5) or HE-AACv2 (29) track
// still reads back as CodecAACLC, with rejection left to the decoder. Making the
// reader reject HE-AAC would turn this test red on purpose, which is the point:
// that change must be a conscious edit here, not silent.
func TestReaderLabelsHEAACAsAACLC(t *testing.T) {
	base := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(4))

	for _, tc := range []struct {
		name string
		aot  byte
	}{
		{"HE-AAC (SBR, AOT 5)", 5},
		{"HE-AACv2 (PS, AOT 29)", 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := patchASCObjectType(t, base, ascMono48k, tc.aot)
			rd, err := NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewReader on patched %s = %v, want it accepted as a container", tc.name, err)
			}
			if got := rd.Info().Codec; got != CodecAACLC {
				t.Fatalf("Info.Codec = %v, want CodecAACLC (reader is profile-agnostic)", got)
			}
		})
	}
}
