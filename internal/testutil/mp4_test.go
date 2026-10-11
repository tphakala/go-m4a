// SPDX-License-Identifier: MIT

package testutil

import (
	"encoding/binary"
	"testing"
)

func box(typ string, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	b = append(b, typ...)
	return append(b, payload...)
}

func TestMDATPayload(t *testing.T) {
	// The ftyp payload spells "mdat" so a byte scan would stop there; the box
	// walk must skip it and return the real mdat payload.
	buf := box("ftyp", []byte("mdatmdat"))
	buf = append(buf, box("mdat", []byte{1, 2, 3})...)
	buf = append(buf, box("moov", nil)...)

	start, end := MDATPayload(t, buf)
	if got := buf[start:end]; string(got) != "\x01\x02\x03" {
		t.Fatalf("MDATPayload = [%d:%d] %v, want the mdat payload {1, 2, 3}", start, end, got)
	}
}

func TestMDATPayloadLargesize(t *testing.T) {
	payload := []byte{9, 8}
	buf := binary.BigEndian.AppendUint32(nil, 1)
	buf = append(buf, "mdat"...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(16+len(payload)))
	buf = append(buf, payload...)

	start, end := MDATPayload(t, buf)
	if start != 16 || end != len(buf) {
		t.Fatalf("MDATPayload = [%d:%d], want [16:%d]", start, end, len(buf))
	}
}
