// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"errors"
	"testing"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// Complete AudioSpecificConfigs for the HE-AAC passthrough test, each a whole
// explicit-hierarchical config that go-aac parses without overreading a truncated
// buffer:
//
//	ascAACLC:   audio object type 2 (AAC-LC), samplingFrequencyIndex 3 (48 kHz),
//	            channelConfiguration 1, plus a zero GASpecificConfig padding byte.
//	ascHEAAC:   audio object type 5 (SBR), sfi 3, chanCfg 1, extensionSFI 3, base AOT 2.
//	ascHEAACv2: audio object type 29 (PS), sfi 3, chanCfg 1, extensionSFI 3, base AOT 2.
//
// The container is built with ascAACLC (the writer accepts only an AAC-LC object
// type) and then rewritten in place to an HE-AAC config, which keeps the esds
// descriptor length and enclosing box sizes unchanged.
var (
	ascAACLC   = []byte{0x11, 0x88, 0x00}
	ascHEAAC   = []byte{0x29, 0x89, 0x88}
	ascHEAACv2 = []byte{0xe9, 0x89, 0x88}
)

// buildAACContainer writes a minimal valid AAC-LC .m4a carrying asc. The frame
// bytes are arbitrary: NewDecoder rejects an HE-AAC stream while parsing the ASC,
// before any access unit is read, so the payload never has to be real AAC.
func buildAACContainer(t *testing.T, asc []byte) []byte {
	t.Helper()
	var ws memWriteSeeker
	w, err := m4a.NewWriter(&ws, m4a.WriterConfig{SampleRate: 48000, Channels: 1, ASC: asc})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame([]byte{0x01, 0x02, 0x03, 0x04}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return ws.buf
}

// rewriteASC returns a copy of an AAC .m4a with its AudioSpecificConfig replaced by
// want, which must be the same length as have. It locates the ASC through the esds
// DecoderSpecificInfo descriptor (tag 0x05, minimal size byte, then the ASC bytes)
// that go-m4a's writer emits, and fails if that descriptor is not present exactly
// once.
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

// TestNewDecoderHEAACSentinels pins the typed HE-AAC passthrough documented on
// NewDecoder: the container reader labels an HE-AAC track as AAC-LC, and the
// decoder is what rejects it, so NewDecoder returns an error matching
// aacpcm.ErrUnsupportedSBR for HE-AAC and aacpcm.ErrUnsupportedPS for HE-AACv2,
// passed through unchanged so errors.Is still matches. The fixtures encode a real
// AAC-LC container and rewrite its ASC to a complete SBR (object type 5) or PS
// (object type 29) config. Changing NewDecoder to wrap the decoder error with %v
// instead of returning it directly turns this test red.
func TestNewDecoderHEAACSentinels(t *testing.T) {
	base := buildAACContainer(t, ascAACLC)

	tests := []struct {
		name   string
		asc    []byte
		wantPS bool // HE-AACv2: errors.Is(err, ErrUnsupportedPS) must hold
	}{
		{"HE-AAC (SBR, AOT 5)", ascHEAAC, false},
		{"HE-AACv2 (PS, AOT 29)", ascHEAACv2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := rewriteASC(t, base, ascAACLC, tc.asc)

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
