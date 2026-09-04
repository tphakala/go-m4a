// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// topLevelBoxBodyLen returns the body length (total minus header) of the first
// top-level box of the given type in data, walking the ISO-BMFF box list the same
// way scanTopLevel does. It handles the 32-bit size, the 64-bit largesize form
// (size field 1, used by the writer's mdat), and the to-end form (size 0). It
// fails the test if the box is not found.
func topLevelBoxBodyLen(t *testing.T, data []byte, want string) int64 {
	t.Helper()
	for off := int64(0); off+8 <= int64(len(data)); {
		total := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		headerLen := int64(8)
		switch total {
		case 1: // 64-bit largesize
			if off+16 > int64(len(data)) {
				t.Fatalf("largesize box %q at %d truncated", typ, off)
			}
			total = int64(binary.BigEndian.Uint64(data[off+8:]))
			headerLen = 16
		case 0: // extends to end of stream
			total = int64(len(data)) - off
		}
		if total < headerLen || off+total > int64(len(data)) {
			t.Fatalf("box %q at %d has bad size %d", typ, off, total)
		}
		if typ == want {
			return total - headerLen
		}
		off += total
	}
	t.Fatalf("no top-level %q box in %d bytes", want, len(data))
	return 0
}

// TestMaxBoxBufferPlainPath pins the readSection box-buffer guard on the plain
// (moov) path: a body larger than the limit is rejected with ErrBoxTooLarge, the
// limit is inclusive (a body exactly at the limit reads), and 0 disables the cap.
// Deleting the limit branch in readSection makes the moovBody-1 case read
// successfully, turning this test red.
func TestMaxBoxBufferPlainPath(t *testing.T) {
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(5))
	moovBody := topLevelBoxBodyLen(t, data, "moov")
	ftypBody := topLevelBoxBodyLen(t, data, "ftyp")
	// The rejection at moovBody-1 must be the moov, so ftyp has to fit under it.
	if ftypBody > moovBody-1 {
		t.Fatalf("ftyp body %d does not fit under moov body %d-1", ftypBody, moovBody)
	}

	// One byte under the moov body: rejected, and specifically as ErrBoxTooLarge.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(moovBody-1)); !errors.Is(err, ErrBoxTooLarge) {
		t.Fatalf("WithMaxBoxBuffer(%d) = %v, want ErrBoxTooLarge", moovBody-1, err)
	}
	// A limit that is not corruption must not masquerade as one.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(moovBody-1)); errors.Is(err, ErrCorrupt) {
		t.Fatal("box-buffer rejection wrongly matches ErrCorrupt")
	}
	// Exactly at the moov body: the limit is inclusive (rejects n > limit), so it reads.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(moovBody)); err != nil {
		t.Fatalf("WithMaxBoxBuffer(%d) (exact moov body) = %v, want success", moovBody, err)
	}
	// 0 disables the cap entirely.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(0)); err != nil {
		t.Fatalf("WithMaxBoxBuffer(0) = %v, want success", err)
	}
	// The default (no option) reads this small file.
	if _, err := NewReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("NewReader default = %v, want success", err)
	}
}

// TestMaxBoxBufferFragmentedPath pins the box-buffer guard on the fragmented
// (moof) path in buildFragmentGeometry. The segment is sized so the single moof
// body is the largest box, above the init moov, so a limit between the two isolates
// the moof guard: it rejects the moof while the init moov reads. Deleting the moof
// guard in buildFragmentGeometry makes the moofBody-1 case succeed, turning this
// test red.
func TestMaxBoxBufferFragmentedPath(t *testing.T) {
	// Many frames in one segment inflate the trun so the moof body exceeds the init
	// moov body, letting a limit sit between them.
	frames := synthFrames(300)
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{uniformSegment(frames, 1024)})

	moovBody := topLevelBoxBodyLen(t, data, "moov")
	moofBody := topLevelBoxBodyLen(t, data, "moof")
	if moovBody > moofBody-1 {
		t.Fatalf("init moov body %d is not below moof body %d-1; raise the frame count", moovBody, moofBody)
	}

	// One byte under the moof body: the init moov reads, the moof is rejected.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(moofBody-1)); !errors.Is(err, ErrBoxTooLarge) {
		t.Fatalf("WithMaxBoxBuffer(%d) = %v, want ErrBoxTooLarge on the moof body", moofBody-1, err)
	}
	// Exactly at the moof body: inclusive, so the whole stream reads.
	if _, err := NewReader(bytes.NewReader(data), WithMaxBoxBuffer(moofBody)); err != nil {
		t.Fatalf("WithMaxBoxBuffer(%d) (exact moof body) = %v, want success", moofBody, err)
	}
	// The default reads this small stream.
	if _, err := NewReader(bytes.NewReader(data)); err != nil {
		t.Fatalf("NewReader default = %v, want success", err)
	}
}
