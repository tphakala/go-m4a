// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"fmt"
	"testing"
)

// ascFor builds an AAC-LC AudioSpecificConfig for the given rate and channel
// count: audioObjectType 2 in the top five bits, then the four-bit sampling
// frequency index, then the four-bit channel configuration. It fails the test
// for a rate the table cannot express, so a typo in a case shows up as a broken
// fixture rather than as a rejection the test then mistakes for the contract.
func ascFor(t *testing.T, rate, channels int) []byte {
	t.Helper()
	sfi := -1
	for i, r := range samplingFrequencyTable {
		if r == rate {
			sfi = i
			break
		}
	}
	if sfi < 0 {
		t.Fatalf("no AAC sampling frequency index for %d Hz", rate)
	}
	return []byte{
		byte(audioObjectTypeAACLC<<3) | byte(sfi>>1),
		byte(sfi&1)<<7 | byte(channels)<<3,
	}
}

// flacStreamInfo is a STREAMINFO block of the right length. The writer copies it
// into dfLa verbatim and never parses it, so the contents do not matter here.
func flacStreamInfo() []byte { return make([]byte, 34) }

// TestAcceptedSampleRates pins which sample rates each codec accepts, which is
// the contract three separate places used to state three different ways (#12).
// The rule is per codec, so quoting one range for the package is what went wrong
// before: AAC is restricted to the sampling-frequency table, Opus is pinned to
// the encapsulation's fixed timescale, and FLAC carries its rate in STREAMINFO
// and is bounded only by what the sample entry can represent.
func TestAcceptedSampleRates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cfg    WriterConfig
		accept bool
	}{
		// AAC-LC: every table rate that fits the 16-bit sample-entry field.
		{"aac 7350 (table floor)", WriterConfig{SampleRate: 7350, Channels: 1, ASC: ascFor(t, 7350, 1)}, true},
		{"aac 8000", WriterConfig{SampleRate: 8000, Channels: 1, ASC: ascFor(t, 8000, 1)}, true},
		{"aac 11025", WriterConfig{SampleRate: 11025, Channels: 1, ASC: ascFor(t, 11025, 1)}, true},
		{"aac 12000", WriterConfig{SampleRate: 12000, Channels: 1, ASC: ascFor(t, 12000, 1)}, true},
		{"aac 16000", WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascFor(t, 16000, 1)}, true},
		{"aac 22050", WriterConfig{SampleRate: 22050, Channels: 2, ASC: ascFor(t, 22050, 2)}, true},
		{"aac 24000", WriterConfig{SampleRate: 24000, Channels: 2, ASC: ascFor(t, 24000, 2)}, true},
		{"aac 32000", WriterConfig{SampleRate: 32000, Channels: 2, ASC: ascFor(t, 32000, 2)}, true},
		{"aac 44100", WriterConfig{SampleRate: 44100, Channels: 2, ASC: ascStereo44k}, true},
		{"aac 48000", WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, true},
		{"aac 64000 (table ceiling that fits)", WriterConfig{SampleRate: 64000, Channels: 2, ASC: ascFor(t, 64000, 2)}, true},
		// Above the sample entry's 16-bit field, so rejected before the table is
		// consulted at all: these two are in the table but cannot be written.
		{"aac 88200 exceeds the sample entry", WriterConfig{SampleRate: 88200, Channels: 2, ASC: ascFor(t, 88200, 2)}, false},
		{"aac 96000 exceeds the sample entry", WriterConfig{SampleRate: 96000, Channels: 2, ASC: ascFor(t, 96000, 2)}, false},
		// Off the table entirely: AAC cannot encode a rate it has no index for.
		{"aac 47999 is not a table rate", WriterConfig{SampleRate: 47999, Channels: 1, ASC: ascMono48k}, false},

		// Opus: the encapsulation fixes the container timescale at 48 kHz.
		{"opus 48000", WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 2}, true},
		{"opus 44100 is not the Opus timescale", WriterConfig{Codec: CodecOpus, SampleRate: 44100, Channels: 2}, false},
		{"opus 16000 is not the Opus timescale", WriterConfig{Codec: CodecOpus, SampleRate: 16000, Channels: 1}, false},

		// FLAC: no rate table, so anything the sample entry can hold.
		{"flac 44100", WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 2, STREAMINFO: flacStreamInfo()}, true},
		{"flac 8000", WriterConfig{Codec: CodecFLAC, SampleRate: 8000, Channels: 1, STREAMINFO: flacStreamInfo()}, true},
		{"flac 47999 needs no table entry", WriterConfig{Codec: CodecFLAC, SampleRate: 47999, Channels: 1, STREAMINFO: flacStreamInfo()}, true},
		{"flac 65535 at the sample entry ceiling", WriterConfig{Codec: CodecFLAC, SampleRate: 65535, Channels: 1, STREAMINFO: flacStreamInfo()}, true},
		{"flac 65536 exceeds the sample entry", WriterConfig{Codec: CodecFLAC, SampleRate: 65536, Channels: 1, STREAMINFO: flacStreamInfo()}, false},

		// The floor is shared by every codec.
		{"zero rate", WriterConfig{Codec: CodecFLAC, SampleRate: 0, Channels: 1, STREAMINFO: flacStreamInfo()}, false},
		{"negative rate", WriterConfig{Codec: CodecFLAC, SampleRate: -48000, Channels: 1, STREAMINFO: flacStreamInfo()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewWriter(&memWS{}, tc.cfg)
			switch {
			case tc.accept && err != nil:
				t.Errorf("NewWriter(%d Hz) = %v, want it accepted", tc.cfg.SampleRate, err)
			case !tc.accept && err == nil:
				t.Errorf("NewWriter(%d Hz) was accepted, want a rejection", tc.cfg.SampleRate)
			}
		})
	}
}

// TestRoundTripSampleRates writes and reads back at every AAC rate the writer
// accepts, so the claim that they are supported is backed by a file that comes
// out the other side rather than by validateConfig letting them past. Before
// this, only 44100 and 48000 had round-trip coverage while the accepted set was
// eleven rates wide.
func TestRoundTripSampleRates(t *testing.T) {
	t.Parallel()
	rates := []int{7350, 8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000}
	for _, rate := range rates {
		t.Run(fmt.Sprintf("aac%d", rate), func(t *testing.T) {
			t.Parallel()
			const channels = 1
			frames := synthFrames(4)
			cfg := WriterConfig{SampleRate: rate, Channels: channels, ASC: ascFor(t, rate, channels)}

			r := readerFromWriter(t, cfg, frames)
			info := r.Info()
			if info.SampleRate != rate {
				t.Errorf("SampleRate = %d, want %d", info.SampleRate, rate)
			}
			if info.Channels != channels {
				t.Errorf("Channels = %d, want %d", info.Channels, channels)
			}
			if info.FrameCount != len(frames) {
				t.Errorf("FrameCount = %d, want %d", info.FrameCount, len(frames))
			}
			got := collectFrames(t, r)
			if len(got) != len(frames) {
				t.Fatalf("read %d frames, want %d", len(got), len(frames))
			}
			for i := range frames {
				if !bytesEqual(got[i], frames[i]) {
					t.Errorf("frame %d differs after the round trip", i)
				}
			}
		})
	}
}

// TestRoundTripFLACOffTableRate pins the asymmetry the prose kept getting wrong:
// a FLAC track carries its rate in STREAMINFO, so the writer accepts rates that
// AAC has no index for, and the reader reports them back unchanged.
func TestRoundTripFLACOffTableRate(t *testing.T) {
	t.Parallel()
	const rate = 47999
	if aacRateSupported(rate) {
		t.Fatalf("%d Hz is in the AAC table, so this case no longer tests an off-table rate", rate)
	}
	cfg := WriterConfig{Codec: CodecFLAC, SampleRate: rate, Channels: 1, STREAMINFO: flacStreamInfo(), EncoderDelay: NoEdit}

	// FLAC access units carry their own block size, so the writer wants the
	// duration per frame rather than inferring one.
	ws := &memWS{}
	w, err := NewWriter(ws, cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, au := range synthFrames(3) {
		if err := w.WriteFrameDuration(au, 4096); err != nil {
			t.Fatalf("WriteFrameDuration %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(ws.buf))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if info := r.Info(); info.SampleRate != rate {
		t.Errorf("SampleRate = %d, want %d", info.SampleRate, rate)
	}
}

// bytesEqual is a local comparison so the table above does not pull bytes into
// this file for one call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
