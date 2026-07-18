// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"testing"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// TestEncodeInterleavedErrors covers the two input-rejection paths of
// EncodeInterleaved: a PCM buffer that is not a whole number of interleaved
// samples, and a sample rate with no AAC AudioSpecificConfig. Both must return an
// error rather than write a malformed file or panic.
func TestEncodeInterleavedErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  aacpcm.Config
		pcm  []byte
	}{
		{
			// stride is 2 bytes (mono S16); a 5-byte buffer is one and a half
			// samples, so the whole-sample check must reject it.
			name: "odd length pcm",
			cfg:  aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1},
			pcm:  []byte{0, 1, 2, 3, 4},
		},
		{
			// 12345 Hz is not an AAC rate, so encoding must fail before any bytes
			// are written.
			name: "unsupported sample rate",
			cfg:  aacpcm.Config{SampleRate: 12345, BitDepth: 16, Channels: 1},
			pcm:  make([]byte, 64),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ws memWriteSeeker
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("panic in EncodeInterleaved: %v", p)
					}
				}()
				return EncodeInterleaved(&ws, tc.cfg, tc.pcm)
			}()
			if err == nil {
				t.Fatal("EncodeInterleaved succeeded, want an error")
			}
		})
	}
}

// TestNewDecoderGarbage confirms NewDecoder over a stream that is not an MP4/M4A
// container returns an error (from the underlying reader) rather than panicking.
func TestNewDecoderGarbage(t *testing.T) {
	garbage := bytes.NewReader([]byte("this is definitely not an mp4 container"))

	dec, _, err := func() (*aacpcm.Decoder, m4a.Info, error) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic in NewDecoder on garbage input: %v", p)
			}
		}()
		return NewDecoder(garbage)
	}()
	if err == nil {
		t.Fatal("NewDecoder over garbage succeeded, want an error")
	}
	if dec != nil {
		t.Errorf("NewDecoder returned a non-nil decoder alongside the error")
	}
}
