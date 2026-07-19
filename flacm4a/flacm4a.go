// SPDX-License-Identifier: MIT

// Package flacm4a is an optional convenience bridge that couples go-flac's FLAC
// codec to the go-m4a container. EncodeInterleaved takes interleaved PCM straight
// to a FLAC .mp4 (fLaC/dfLa); DecodeInterleaved opens one and returns the decoded
// PCM. It is a thin seam over the two libraries: the core m4a package stays
// stdlib-only, and only this subpackage imports go-flac, so a consumer that merely
// muxes or demuxes never pulls the codec.
package flacm4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	flacpcm "github.com/tphakala/go-flac/pcm"

	m4a "github.com/tphakala/go-m4a"
)

// Config configures FLAC encoding. SampleRate is the audio rate in Hz; Channels is
// 1 or 2; BitDepth is 16 or 24; CompressionLevel is 0 (fastest) to 8 (smallest).
type Config struct {
	SampleRate       int
	Channels         int
	BitDepth         int
	CompressionLevel int
}

// EncodeInterleaved encodes interleaved little-endian PCM as a FLAC .mp4 to w. FLAC
// is lossless and has no encoder priming, so no edit list is written and a decode
// reproduces the input exactly.
func EncodeInterleaved(w io.WriteSeeker, cfg Config, pcm []byte) error {
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return fmt.Errorf("go-m4a/flacm4a: channels %d out of range, want 1 or 2", cfg.Channels)
	}
	// Require a byte-aligned bit depth so the interleaved-PCM stride is exact; a
	// non-byte-aligned depth (for example 20) would divide to the wrong stride and
	// mis-parse the buffer before go-flac ever saw the config.
	if cfg.BitDepth != 16 && cfg.BitDepth != 24 {
		return fmt.Errorf("go-m4a/flacm4a: bit depth %d unsupported, want 16 or 24", cfg.BitDepth)
	}
	stride := cfg.Channels * (cfg.BitDepth / 8)
	if len(pcm)%stride != 0 {
		return fmt.Errorf("go-m4a/flacm4a: PCM length %d is not a whole number of %d-byte interleaved samples", len(pcm), stride)
	}
	samplesPerChannel := len(pcm) / stride

	fe, err := flacpcm.NewFrameEncoder(flacpcm.Config{
		SampleRate:       cfg.SampleRate,
		Channels:         cfg.Channels,
		BitDepth:         cfg.BitDepth,
		CompressionLevel: cfg.CompressionLevel,
		TotalSamples:     uint64(samplesPerChannel),
	})
	if err != nil {
		return fmt.Errorf("go-m4a/flacm4a: new frame encoder: %w", err)
	}

	// Encode all frames up front so StreamInfoBytes carries the measured min/max
	// frame sizes and MD5 before the dfLa box is built at Close.
	type frame struct {
		data      []byte
		blockSize int
	}
	var frames []frame
	err = fe.EncodeInterleaved(pcm, func(fr []byte, blockSize int) error {
		frames = append(frames, frame{data: bytes.Clone(fr), blockSize: blockSize})
		return nil
	})
	if err != nil {
		return fmt.Errorf("go-m4a/flacm4a: encode: %w", err)
	}

	wr, err := m4a.NewWriter(w, m4a.WriterConfig{
		Codec:      m4a.CodecFLAC,
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		STREAMINFO: fe.StreamInfoBytes(),
		// FLAC frames decode to exactly the input samples with no priming, so there
		// is nothing to trim: omit the edit list.
		EncoderDelay: m4a.NoEdit,
	})
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := wr.WriteFrameDuration(f.data, uint32(f.blockSize)); err != nil {
			return err
		}
	}
	return wr.Close()
}

// DecodeInterleaved opens a FLAC .mp4 and decodes it to interleaved little-endian
// PCM, returning the PCM together with the container Info. FLAC is lossless, so the
// PCM is bit-identical to what EncodeInterleaved was given.
func DecodeInterleaved(r io.ReadSeeker) ([]byte, m4a.Info, error) {
	rd, err := m4a.NewReader(r)
	if err != nil {
		return nil, m4a.Info{}, err
	}
	info := rd.Info()
	if info.Codec != m4a.CodecFLAC {
		return nil, info, fmt.Errorf("go-m4a/flacm4a: track codec is %v, not FLAC", info.Codec)
	}

	fd, err := flacpcm.NewFrameDecoder(info.CodecConfig)
	if err != nil {
		return nil, info, fmt.Errorf("go-m4a/flacm4a: new frame decoder: %w", err)
	}

	var out []byte
	for {
		au, err := rd.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, info, err
		}
		pcm, _, err := fd.DecodeInterleaved(au)
		if err != nil {
			return nil, info, fmt.Errorf("go-m4a/flacm4a: decode frame: %w", err)
		}
		out = append(out, pcm...)
	}
	return out, info, nil
}
