// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"testing"

	"github.com/tphakala/go-m4a/internal/box"
)

// buildEmptyOpusFile assembles a structurally valid Opus .mp4 whose track carries
// no access units: the sample tables are all empty and there is no mdat payload.
// This package's own writer refuses to close such a file, so it can only arrive
// from a foreign muxer; the dOps carries real fields so the decoder still builds.
func buildEmptyOpusFile(t *testing.T) []byte {
	t.Helper()
	const (
		sampleRate = 48000
		channels   = 1
		preSkip    = 312
	)

	dOps := box.AppendDops(nil, channels, preSkip, sampleRate)
	stbl := box.AppendStsdEntry(nil, box.AppendOpusEntry(nil, channels, sampleRate, dOps))
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
	file := buildEmptyOpusFile(t)

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
