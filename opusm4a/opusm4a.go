// SPDX-License-Identifier: MIT

// Package opusm4a is an optional convenience bridge that couples go-opus's Opus
// codec to the go-m4a container. EncodeInterleaved takes interleaved 16-bit PCM
// straight to a gapless Opus .mp4; DecodeInterleaved opens one and returns the
// decoded PCM. It is a thin seam over the two libraries: the core m4a package
// stays stdlib-only, and only this subpackage imports go-opus, so a consumer that
// merely muxes or demuxes never pulls the codec.
package opusm4a

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/tphakala/go-opus/opus"

	m4a "github.com/tphakala/go-m4a"
)

// opusRate is the sample rate the Opus-in-ISOBMFF encapsulation fixes for the
// container timescale, and the rate this bridge encodes and decodes at.
const opusRate = 48000

// frameSamplesPerChannel is the per-channel sample count of one encoded packet: a
// 20 ms frame at 48 kHz, the standard Opus-in-MP4 packetization.
const frameSamplesPerChannel = opusRate / 50 // 960

// Config configures Opus encoding. SampleRate must be 48000 (the Opus container
// rate); Channels is 1 or 2; Bitrate is the target bits per second (0 selects the
// go-opus automatic rate).
type Config struct {
	SampleRate int
	Channels   int
	Bitrate    int
}

// EncodeInterleaved encodes interleaved little-endian 16-bit PCM as a gapless Opus
// .mp4 to w. It splits the input into 20 ms frames (padding the final frame with
// silence), encodes each to one Opus packet, and writes an edit list that trims
// the encoder pre-skip and the trailing padding so a compliant player presents
// exactly the original audio.
func EncodeInterleaved(w io.WriteSeeker, cfg Config, pcm []byte) error {
	if cfg.SampleRate != opusRate {
		return fmt.Errorf("go-m4a/opusm4a: SampleRate %d unsupported, want %d", cfg.SampleRate, opusRate)
	}
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return fmt.Errorf("go-m4a/opusm4a: channels %d out of range, want 1 or 2", cfg.Channels)
	}
	stride := 2 * cfg.Channels // bytes per interleaved sample (S16)
	if len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/opusm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}
	samplesPerChannel := len(pcm) / stride

	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		Bitrate:    cfg.Bitrate,
	})
	if err != nil {
		return fmt.Errorf("go-m4a/opusm4a: new encoder: %w", err)
	}
	preSkip := enc.PreSkip()

	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		Codec:               m4a.CodecOpus,
		SampleRate:          opusRate,
		Channels:            cfg.Channels,
		OpusPreSkip:         preSkip,
		OpusInputSampleRate: opusRate,
		MediaLength:         int64(samplesPerChannel),
	})
	if err != nil {
		return err
	}

	// The encoder has a preSkip-sample algorithmic delay, so the last preSkip
	// content samples only emerge after that many more input samples. Encode
	// samplesPerChannel+preSkip samples (zero-padding past the input) so every
	// content sample is flushed out; the edit list trims the leading pre-skip and
	// the trailing padding.
	total := samplesPerChannel + preSkip
	frameLen := frameSamplesPerChannel * cfg.Channels // int16 values per frame
	frame := make([]int16, frameLen)
	pkt := make([]byte, 1276) // holds any single VBR Opus packet
	for off := 0; off < total; off += frameSamplesPerChannel {
		// Fill this frame's interleaved samples into the reused int16 buffer,
		// zero-padding a short final frame and the trailing flush frames so every
		// packet is a full 20 ms.
		for i := range frame {
			frame[i] = 0
		}
		remaining := samplesPerChannel - off
		if remaining < 0 {
			remaining = 0
		}
		n := min(frameSamplesPerChannel, remaining) * cfg.Channels
		base := off * stride
		for i := 0; i < n; i++ {
			frame[i] = int16(binary.LittleEndian.Uint16(pcm[base+2*i:]))
		}
		m, err := enc.Encode(frame, pkt)
		if err != nil {
			return fmt.Errorf("go-m4a/opusm4a: encode frame at sample %d: %w", off, err)
		}
		dur, err := opus.PacketDuration(pkt[:m])
		if err != nil {
			return fmt.Errorf("go-m4a/opusm4a: packet duration: %w", err)
		}
		if err := wr.WriteFrameDuration(pkt[:m], uint32(dur)); err != nil {
			return err
		}
	}
	return wr.Close()
}

// DecodeInterleaved opens an Opus .mp4 and decodes it to interleaved little-endian
// 16-bit PCM at 48 kHz, returning the PCM together with the container Info. The
// returned PCM includes the leading pre-skip priming and the trailing final-frame
// padding: for sample-accurate output, skip Info.EncoderDelay leading samples per
// channel, then keep Info.Duration-worth of samples (Duration times 48000 per
// channel) and discard the rest.
func DecodeInterleaved(r io.ReadSeeker) ([]byte, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, m4a.Info{}, err
	}
	info := rd.Info()
	if info.Codec != m4a.CodecOpus {
		return nil, info, fmt.Errorf("go-m4a/opusm4a: track codec is %v, not Opus", info.Codec)
	}
	channels := info.Channels
	if channels < 1 || channels > 2 {
		return nil, info, fmt.Errorf("go-m4a/opusm4a: unsupported channel count %d", channels)
	}

	dec, err := opus.NewDecoder(opusRate, channels)
	if err != nil {
		return nil, info, fmt.Errorf("go-m4a/opusm4a: new decoder: %w", err)
	}

	var out []byte
	// One packet decodes to at most 120 ms (5760 samples per channel) of PCM.
	buf := make([]int16, 5760*channels)
	for {
		au, err := rd.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, info, err
		}
		n, err := dec.Decode(au, buf)
		if err != nil {
			return nil, info, fmt.Errorf("go-m4a/opusm4a: decode packet: %w", err)
		}
		for i := 0; i < n*channels; i++ {
			out = binary.LittleEndian.AppendUint16(out, uint16(buf[i]))
		}
	}
	return out, info, nil
}
