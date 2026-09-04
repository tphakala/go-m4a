// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"errors"
	"testing"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// patchASCObjectType returns a copy of an AAC .m4a with the AudioSpecificConfig
// audio object type (the top 5 bits of the first ASC byte) set to aot. It locates
// the ASC through the esds DecoderSpecificInfo descriptor (tag 0x05, minimal size,
// then the ASC bytes) that go-m4a's writer emits, and fails if it is not present
// exactly once.
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

// TestNewDecoderHEAACSentinels pins the typed HE-AAC passthrough documented on
// NewDecoder: the container reader labels an HE-AAC track as AAC-LC, and the
// decoder is what rejects it, surfacing aacpcm.ErrUnsupportedSBR for HE-AAC and
// aacpcm.ErrUnsupportedPS for HE-AACv2, unwrapped through the bridge so errors.Is
// still matches. The fixtures are synthesized by encoding a real AAC-LC .m4a and
// patching the ASC audio object type to 5 (SBR) and 29 (PS). Changing NewDecoder
// to wrap the decoder error with %v instead of returning it directly turns this
// test red.
func TestNewDecoderHEAACSentinels(t *testing.T) {
	cfg := aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	base := encodeToBytes(t, cfg, make([]byte, 4096*2)) // 4096 mono S16 samples

	// Recover the real ASC from the container so the patch locator is exact.
	rd, err := m4a.NewReader(bytes.NewReader(base))
	if err != nil {
		t.Fatalf("NewReader on the base AAC-LC file: %v", err)
	}
	asc := rd.Info().ASC
	if len(asc) < 2 {
		t.Fatalf("base ASC too short: % x", asc)
	}

	tests := []struct {
		name   string
		aot    byte
		wantPS bool // HE-AACv2: errors.Is(err, ErrUnsupportedPS) must hold
	}{
		{"HE-AAC (SBR, AOT 5)", 5, false},
		{"HE-AACv2 (PS, AOT 29)", 29, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := patchASCObjectType(t, base, asc, tc.aot)

			_, _, err := func() (_ *aacpcm.Decoder, _ m4a.Info, err error) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("panic in NewDecoder on %s: %v", tc.name, p)
					}
				}()
				return NewDecoder(bytes.NewReader(data))
			}()
			if err == nil {
				t.Fatalf("NewDecoder on %s succeeded, want an HE-AAC rejection", tc.name)
			}
			// The whole HE-AAC family matches ErrUnsupportedSBR (PS wraps SBR), and
			// everything matches the base ErrUnsupported.
			if !errors.Is(err, aacpcm.ErrUnsupportedSBR) {
				t.Errorf("err = %v, want errors.Is(err, ErrUnsupportedSBR)", err)
			}
			if !errors.Is(err, aacpcm.ErrUnsupported) {
				t.Errorf("err = %v, want errors.Is(err, ErrUnsupported)", err)
			}
			// ErrUnsupportedPS distinguishes HE-AACv2 from plain HE-AAC.
			if got := errors.Is(err, aacpcm.ErrUnsupportedPS); got != tc.wantPS {
				t.Errorf("errors.Is(err, ErrUnsupportedPS) = %v, want %v (err = %v)", got, tc.wantPS, err)
			}
		})
	}
}
