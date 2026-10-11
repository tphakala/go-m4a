// SPDX-License-Identifier: MIT

package testutil

import (
	"encoding/binary"
	"testing"
)

// MDATPayload returns the byte range of the mdat payload (the raw access units)
// in an encoded .mp4 buffer. It walks the top-level box headers rather than
// scanning for "mdat", which could match arbitrary payload bytes, and fails the
// test on a malformed header or a buffer with no mdat.
func MDATPayload(tb testing.TB, buf []byte) (start, end int) {
	tb.Helper()
	for off := 0; off+8 <= len(buf); {
		size := int(binary.BigEndian.Uint32(buf[off:]))
		hdr := 8
		if size == 1 {
			if off+16 > len(buf) {
				tb.Fatalf("truncated largesize header at %d", off)
			}
			size = int(binary.BigEndian.Uint64(buf[off+8:]))
			hdr = 16
		}
		if size < hdr || off+size > len(buf) {
			tb.Fatalf("box at %d has bad size %d", off, size)
		}
		if string(buf[off+4:off+8]) == "mdat" {
			return off + hdr, off + size
		}
		off += size
	}
	tb.Fatal("no mdat box in encoded buffer")
	return 0, 0
}
