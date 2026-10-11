// SPDX-License-Identifier: MIT

package testutil

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
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

// stopTB records a fatal call and ends the goroutine, as testing.T's Fatal and
// Fatalf do, so a test can observe MDATPayload rejecting a buffer.
type stopTB struct {
	testing.TB

	fatal string
}

func (*stopTB) Helper() {}

func (s *stopTB) Fatal(args ...any) {
	s.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (s *stopTB) Fatalf(format string, args ...any) {
	s.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestMDATPayloadRejectsOversizedBox(t *testing.T) {
	// An mdat after an 8-byte box declaring a largesize of MaxInt64 - 7: as an
	// int, off+size wraps negative, so a check written as off+size > len(buf)
	// passes it.
	buf := box("free", nil)
	off := len(buf)
	buf = binary.BigEndian.AppendUint32(buf, 1)
	buf = append(buf, "mdat"...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(math.MaxInt64)-uint64(off)+1)
	buf = append(buf, 0, 0)

	if start, end, fatal := runMDATPayload(buf); fatal == "" {
		t.Fatalf("MDATPayload accepted a box larger than the buffer: [%d:%d]", start, end)
	}
}

func TestMDATPayloadRejectsTruncatedLargesize(t *testing.T) {
	// A size of 1 announces a 64-bit largesize, but the buffer ends first.
	buf := binary.BigEndian.AppendUint32(nil, 1)
	buf = append(buf, "mdat"...)
	buf = append(buf, 0, 0, 0)
	if start, end, fatal := runMDATPayload(buf); fatal == "" {
		t.Fatalf("MDATPayload accepted a truncated largesize header: [%d:%d]", start, end)
	}
}

// runMDATPayload calls MDATPayload with a stopTB on its own goroutine and
// returns the range it gave and the fatal message it recorded, if any.
func runMDATPayload(buf []byte) (start, end int, fatal string) {
	rec := &stopTB{}
	var s, e int
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, e = MDATPayload(rec, buf)
	}()
	<-done
	return s, e, rec.fatal
}
