// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/tphakala/go-m4a/internal/box"
)

// appendConstStsz emits an stsz (sample size) FullBox in the constant-size form:
// a nonzero sample_size shared by every sample and sample_count, with NO
// per-sample list. box.AppendStsz always writes sample_size 0 plus a per-sample
// list, so this path (ParseStsz returning a constant) is unreachable through the
// public marshaler and the box bytes are assembled directly. The body is
// version/flags(4) + sample_size(4) + sample_count(4), so the total box size is
// the 8-byte header plus 12, i.e. 20 bytes.
func appendConstStsz(sampleSize, sampleCount uint32) []byte {
	b := box.AppendFullBoxHeader(nil, 20, box.NewFourCC("stsz"), 0, 0)
	b = binary.BigEndian.AppendUint32(b, sampleSize)  // constant size shared by all samples
	b = binary.BigEndian.AppendUint32(b, sampleCount) // sample_count, no entries follow
	return b
}

// TestReaderConstStsz covers the constant-size stsz reader path (sample_size !=
// 0). The Writer always emits sample_size 0 with a per-sample list, so a
// hand-assembled file is the only way to reach the rd.constSize branch of
// buildGeometry, sizeOf, and ReadFrame. Every access unit is the same size.
func TestReaderConstStsz(t *testing.T) {
	const (
		n         = 6
		constSize = 13
	)

	// N access units, each exactly constSize bytes, with distinct contents so the
	// byte-for-byte check catches an off-by-one in the constant-size cursor math.
	frames := make([][]byte, n)
	payload := make([]byte, 0, n*constSize)
	for i := range frames {
		au := make([]byte, constSize)
		for j := range au {
			au[j] = byte(0xA0 + i*constSize + j)
		}
		frames[i] = au
		payload = append(payload, au...)
	}

	stsz := appendConstStsz(constSize, n)
	offsetBox := func(payloadStart int64) []byte {
		return box.AppendStco(nil, []uint32{uint32(payloadStart)})
	}

	data := handAssembleM4A(48000, 1, ascMono48k, n, payload, stsz, offsetBox)

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	info := r.Info()
	if info.SampleRate != 48000 {
		t.Errorf("SampleRate = %d, want 48000", info.SampleRate)
	}
	if info.Channels != 1 {
		t.Errorf("Channels = %d, want 1", info.Channels)
	}
	if info.FrameCount != n {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, n)
	}

	got := collectFrames(t, r)
	if len(got) != n {
		t.Fatalf("read %d frames, want %d", len(got), n)
	}
	for i := range frames {
		if len(got[i]) != constSize {
			t.Errorf("frame %d length = %d, want %d", i, len(got[i]), constSize)
		}
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("const stsz frame %d = % x, want % x", i, got[i], frames[i])
		}
	}
}
