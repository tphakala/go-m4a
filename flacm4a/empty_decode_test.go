// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"testing"

	"github.com/tphakala/go-m4a/internal/box"
)

// buildEmptyFLACFile assembles a structurally valid FLAC .mp4 whose track carries
// no access units: the sample tables are all empty and there is no mdat payload.
// This package's own writer refuses to close such a file, so it can only arrive
// from a foreign muxer; the STREAMINFO is a real one so the decoder still builds.
func buildEmptyFLACFile(t *testing.T, streamInfo []byte) []byte {
	t.Helper()
	const (
		sampleRate = 44100
		channels   = 1
	)

	stbl := box.AppendStsdEntry(nil, box.AppendFlacEntry(nil, channels, sampleRate, box.AppendDfla(nil, streamInfo)))
	stbl = box.AppendSttsRuns(stbl, nil)
	stbl = box.AppendStsc(stbl, 1, 0, 1)
	stbl = box.AppendStsz(stbl, nil)
	stbl = box.AppendStco(stbl, nil)

	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, stbl)

	var mdia []byte
	mdia = box.AppendMdhd(mdia, sampleRate, 0)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	var trak []byte
	trak = box.AppendTkhd(trak, 1, 0)
	trak = box.AppendMdia(trak, mdia)

	moov := box.AppendMvhd(nil, sampleRate, 0)
	moov = box.AppendTrak(moov, trak)

	out := box.AppendFtyp(nil, box.NewFourCC("M4A "), 0, box.NewFourCC("M4A "), box.NewFourCC("isom"))
	return box.AppendMoov(out, moov)
}

// TestDecodeInterleavedEmptyReturnsNil pins the empty-track contract: a decode
// that produces no audio hands back a nil slice, not a non-nil zero-length one,
// so a caller's pcm == nil check and a JSON marshal both see the absent case as
// absent. Reachable only from a foreign file, since the writer will not produce
// one.
func TestDecodeInterleavedEmptyReturnsNil(t *testing.T) {
	// A real STREAMINFO so the frame decoder builds; the file just carries no frames.
	enc := &memWS{}
	if err := EncodeInterleaved(enc, Config{SampleRate: 44100, Channels: 1, BitDepth: 16, CompressionLevel: 5}, genS16(4096, 1)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	_, _, info0, err := openStream(bytes.NewReader(enc.buf))
	if err != nil {
		t.Fatalf("openStream on the encoded file: %v", err)
	}
	streamInfo := info0.CodecConfig

	file := buildEmptyFLACFile(t, streamInfo)

	pcm, info, err := DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved on an empty track: %v", err)
	}
	if info.FrameCount != 0 {
		t.Errorf("FrameCount = %d, want 0", info.FrameCount)
	}
	if pcm != nil {
		t.Errorf("PCM = %#v (len %d), want nil for an empty decode", pcm, len(pcm))
	}
}
