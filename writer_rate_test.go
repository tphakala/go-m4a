// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
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

// flacStreamInfo builds a STREAMINFO block declaring the given rate. The writer
// copies it into dfLa verbatim and never parses it, so a block of zeros would
// satisfy every check here; it carries the real rate anyway because a decoder
// takes the authoritative rate from STREAMINFO, so a fixture declaring 0 Hz
// would be exactly the divergent file this package cannot detect and the tests
// should not be modelling.
//
// Layout (RFC 9639 section 8.2): 16 bits min block size, 16 max, 24 min frame
// size, 24 max, then 20 bits sample rate, 3 bits channels-1, 5 bits bit
// depth-1, 36 bits total samples, 128 bits MD5. The rate therefore starts at
// bit 160, which is byte 10.
func flacStreamInfo(rate, channels int) []byte {
	si := make([]byte, 34)
	si[10] = byte(rate >> 12)
	si[11] = byte(rate >> 4)
	si[12] = byte(rate<<4) | byte((channels-1)<<1)
	return si
}

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
		{"flac 44100", WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 2, STREAMINFO: flacStreamInfo(44100, 2)}, true},
		{"flac 8000", WriterConfig{Codec: CodecFLAC, SampleRate: 8000, Channels: 1, STREAMINFO: flacStreamInfo(8000, 1)}, true},
		{"flac 47999 needs no table entry", WriterConfig{Codec: CodecFLAC, SampleRate: 47999, Channels: 1, STREAMINFO: flacStreamInfo(47999, 1)}, true},
		{"flac 65535 at the sample entry ceiling", WriterConfig{Codec: CodecFLAC, SampleRate: 65535, Channels: 1, STREAMINFO: flacStreamInfo(65535, 1)}, true},
		{"flac 65536 exceeds the sample entry", WriterConfig{Codec: CodecFLAC, SampleRate: 65536, Channels: 1, STREAMINFO: flacStreamInfo(65536, 1)}, false},

		// The floor is shared by every codec.
		{"zero rate", WriterConfig{Codec: CodecFLAC, SampleRate: 0, Channels: 1, STREAMINFO: flacStreamInfo(48000, 1)}, false},
		{"negative rate", WriterConfig{Codec: CodecFLAC, SampleRate: -48000, Channels: 1, STREAMINFO: flacStreamInfo(48000, 1)}, false},
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
				if !bytes.Equal(got[i], frames[i]) {
					t.Errorf("frame %d differs after the round trip", i)
				}
			}
		})
	}
}

// TestRoundTripFLACOffTableRate pins the asymmetry the prose kept getting wrong:
// FLAC consults no rate table, so the writer accepts rates AAC has no index for.
//
// Note what carries the rate here. A conforming decoder takes it from STREAMINFO,
// but this package's Reader reports the sample entry (reader.go, resolveFormat:
// "FLAC has no ASC: the sample entry samplerate and channel count are
// authoritative"), so what round-trips below is the sample entry. The fixture
// declares the same rate in STREAMINFO so the two agree; a config where they
// disagree produces a file this package and ffmpeg read differently, which
// nothing currently detects.
func TestRoundTripFLACOffTableRate(t *testing.T) {
	t.Parallel()
	const rate = 47999
	if aacRateSupported(rate) {
		t.Fatalf("%d Hz is in the AAC table, so this case no longer tests an off-table rate", rate)
	}
	cfg := WriterConfig{Codec: CodecFLAC, SampleRate: rate, Channels: 1, STREAMINFO: flacStreamInfo(rate, 1), EncoderDelay: NoEdit}

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

// TestSampleEntryCarriesTheRate reads the samplerate straight out of the written
// mp4a AudioSampleEntry.
//
// The round-trip tests cannot cover this field: for AAC the reader prefers the
// rate derived from the ASC, so hardcoding a wrong value into the sample entry
// leaves the whole suite green. That field is the entire reason
// maxAudioSampleEntryRate exists, so it deserves an assertion that looks at the
// bytes rather than at what the reader chose to believe.
func TestSampleEntryCarriesTheRate(t *testing.T) {
	t.Parallel()
	for _, rate := range []int{7350, 44100, 48000, 64000} {
		t.Run(fmt.Sprintf("aac%d", rate), func(t *testing.T) {
			t.Parallel()
			data := writeM4A(t, WriterConfig{SampleRate: rate, Channels: 1, ASC: ascFor(t, rate, 1)}, synthFrames(2))

			// AudioSampleEntry: after the four-byte type come 6 reserved, 2
			// data_reference_index, 8 reserved, 2 channelcount, 2 samplesize, 2
			// pre_defined and 2 reserved, so the 16.16 samplerate starts 28 bytes in.
			i := bytes.Index(data, []byte("mp4a"))
			if i < 0 {
				t.Fatal("no mp4a sample entry in the written file")
			}
			field := binary.BigEndian.Uint32(data[i+28 : i+32])
			if got := int(field >> 16); got != rate {
				t.Errorf("sample entry samplerate = %d, want %d (raw field %#08x)", got, rate, field)
			}
			if low := field & 0xFFFF; low != 0 {
				t.Errorf("sample entry samplerate fraction = %#04x, want 0", low)
			}
		})
	}
}

// TestOffTableRateReportsTheRateNotTheASC pins which error an off-table rate
// gets. The rate-table guard in validateAACConfig cannot change what is accepted,
// since an off-table rate can never match an ASC either, so the only thing it
// contributes is this message. Without an assertion on it, deleting the guard
// leaves the suite green and the caller gets sent looking for an ASC mismatch
// they cannot fix.
func TestOffTableRateReportsTheRateNotTheASC(t *testing.T) {
	t.Parallel()
	_, err := NewWriter(&memWS{}, WriterConfig{SampleRate: 47999, Channels: 1, ASC: ascMono48k})
	if err == nil {
		t.Fatal("NewWriter accepted 47999 Hz, which is not an AAC table rate")
	}
	if !strings.Contains(err.Error(), "unsupported sample rate") {
		t.Errorf("error = %q, want it to name the rate as unsupported rather than blame the ASC", err)
	}
}
