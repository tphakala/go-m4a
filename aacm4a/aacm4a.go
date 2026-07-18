// SPDX-License-Identifier: MIT

// Package aacm4a is an optional convenience bridge that couples go-aac's AAC-LC
// codec to the go-m4a container. EncodeInterleaved takes interleaved PCM straight
// to a gapless .m4a; NewDecoder opens an .m4a and hands back a go-aac decoder
// that streams PCM. It is a thin seam over the two libraries: the core m4a
// package stays stdlib-only, and only this subpackage imports go-aac, so a
// consumer that merely muxes or demuxes never pulls the codec.
package aacm4a

import (
	"bytes"
	"fmt"
	"io"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// audioObjectTypeAACLC is the MPEG-4 Audio Object Type for AAC-LC, the first
// five bits of an AudioSpecificConfig.
const audioObjectTypeAACLC = 2

// adtsHeaderLen is the length in bytes of a CRC-less ADTS frame header. go-aac
// emits ADTS without the optional CRC, so every frame is a 7-byte header
// followed immediately by the raw AAC-LC access unit.
const adtsHeaderLen = 7

// aacSampleRates is the MPEG-4 AudioSpecificConfig samplingFrequencyIndex table
// (ISO/IEC 14496-3 Table 1.16). The index of a rate in this table is its 4-bit
// samplingFrequencyIndex. It matches go-aac's asc.go and the core writer's own
// table.
var aacSampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

// EncodeInterleaved encodes interleaved little-endian signed integer PCM as an
// AAC-LC .m4a to w, with an edit list that trims the encoder priming so the file
// is sample-accurate and gapless. cfg is the same go-aac pcm.Config used for
// ADTS encoding (SampleRate 44100/48000, BitDepth 16/24/32, Channels 1/2).
func EncodeInterleaved(w io.WriteSeeker, cfg aacpcm.Config, pcm []byte) error {
	// Reject a partial trailing sample up front, with this package's prefix, so
	// the message names the container bridge rather than the encoder. A zero or
	// negative stride means cfg itself is invalid; leave that diagnosis to the
	// encoder below, which owns the canonical config error messages.
	stride := cfg.Channels * (cfg.BitDepth / 8)
	if stride > 0 && len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/aacm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}

	// Encode the whole clip to an in-memory ADTS stream. This validates cfg (a
	// bad bit depth, rate, or channel count surfaces here) and produces a
	// self-framing stream, all before anything is written to w.
	var adts bytes.Buffer
	if err := aacpcm.EncodeInterleaved(&adts, cfg, pcm); err != nil {
		return err
	}
	// aacpcm.EncodeInterleaved validated cfg above, so stride is positive here.
	// Guard explicitly anyway so the division can never panic if go-aac's
	// validation ever stops rejecting a zero channel count or sub-byte depth.
	if stride <= 0 {
		return fmt.Errorf("go-m4a/aacm4a: invalid stride for %d channels at %d-bit PCM", cfg.Channels, cfg.BitDepth)
	}
	samplesPerChannel := len(pcm) / stride

	asc, err := audioSpecificConfig(cfg.SampleRate, cfg.Channels)
	if err != nil {
		return err
	}

	// EncoderDelay left 0 selects m4a.DefaultEncoderDelay (1024); MediaLength
	// pins the edit-list segment to the exact source length so trailing padding
	// is excluded too.
	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		SampleRate:  cfg.SampleRate,
		Channels:    cfg.Channels,
		ASC:         asc,
		MediaLength: int64(samplesPerChannel),
	})
	if err != nil {
		return err
	}

	if err := writeADTSFrames(wr, adts.Bytes()); err != nil {
		return err
	}
	return wr.Close()
}

// audioSpecificConfig builds the 2-byte AAC-LC AudioSpecificConfig for the given
// rate and channel count, matching go-aac's asc.go: 5-bit audio object type (2),
// 4-bit samplingFrequencyIndex, 4-bit channelConfiguration, then three zero
// GASpecificConfig bits. For 48000 mono this is {0x11, 0x88}.
func audioSpecificConfig(sampleRate, channels int) ([]byte, error) {
	srIndex := -1
	for i, r := range aacSampleRates {
		if r == sampleRate {
			srIndex = i
			break
		}
	}
	if srIndex < 0 {
		return nil, fmt.Errorf("go-m4a/aacm4a: sample rate %d Hz has no AudioSpecificConfig index", sampleRate)
	}
	v := (audioObjectTypeAACLC << 11) | (srIndex << 7) | (channels << 3)
	return []byte{byte(v >> 8), byte(v)}, nil
}

// writeADTSFrames splits a CRC-less ADTS buffer into frames and writes each
// frame's raw access unit to wr. The ADTS stream comes from go-aac, so a
// malformed frame is an internal invariant break rather than untrusted input;
// every field is still bounds-checked and any inconsistency is returned wrapped.
func writeADTSFrames(wr *m4a.Writer, adts []byte) error {
	for i := 0; i < len(adts); {
		if i+adtsHeaderLen > len(adts) {
			return fmt.Errorf("go-m4a/aacm4a: internal error: truncated ADTS header at offset %d of %d bytes", i, len(adts))
		}
		// Syncword: byte 0 is 0xFF, and the top nibble plus the two layer bits of
		// byte 1 must be 1111x00x, i.e. (byte1 & 0xF6) == 0xF0.
		if adts[i] != 0xff || adts[i+1]&0xf6 != 0xf0 {
			return fmt.Errorf("go-m4a/aacm4a: internal error: no ADTS syncword at offset %d", i)
		}
		// protection_absent (byte 1, bit 0) must be 1: go-aac emits CRC-less
		// frames, so the header is exactly 7 bytes. A 0 here would mean a 9-byte
		// CRC header, invalidating the fixed payload offset below.
		if adts[i+1]&0x01 != 0x01 {
			return fmt.Errorf("go-m4a/aacm4a: internal error: CRC-protected ADTS frame at offset %d", i)
		}
		frameLen := (int(adts[i+3]&0x03) << 11) | (int(adts[i+4]) << 3) | (int(adts[i+5]) >> 5)
		if frameLen < adtsHeaderLen || i+frameLen > len(adts) {
			return fmt.Errorf("go-m4a/aacm4a: internal error: ADTS frame length %d at offset %d overruns %d-byte buffer", frameLen, i, len(adts))
		}
		if err := wr.WriteFrame(adts[i+adtsHeaderLen : i+frameLen]); err != nil {
			return err
		}
		i += frameLen
	}
	return nil
}

// NewDecoder opens an MP4/M4A file and returns a go-aac decoder that streams
// interleaved little-endian S16 PCM, together with the container Info
// (SampleRate, Channels, FrameCount, EncoderDelay, Duration). The go-aac decoder
// is not edit-list aware: it emits every decoded sample (FrameCount*1024 per
// channel), including both the leading priming and the trailing final-frame
// padding. For sample-accurate output, skip Info.EncoderDelay leading samples
// per channel, then keep only as many samples per channel as Info.Duration
// represents (Duration*SampleRate), discarding the trailing padding the edit
// list excludes.
func NewDecoder(r io.ReadSeeker) (*aacpcm.Decoder, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, m4a.Info{}, err
	}
	info := rd.Info()
	d, err := aacpcm.NewDecoder(rd.RawStream(), aacpcm.WithRawStream(info.ASC))
	if err != nil {
		return nil, info, err
	}
	return d, info, nil
}
