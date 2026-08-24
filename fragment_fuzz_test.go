// SPDX-License-Identifier: MIT

package m4a

import (
	"encoding/binary"
	"testing"
)

// segmentExpectation is everything a media segment must say about the access
// units it was given. Checking all of it, rather than just the box framing, is
// what makes the fuzz target able to catch a wrong duration, a stalled decode
// time or a reordered sample table.
type segmentExpectation struct {
	sizes     []uint32
	durations []uint32
	sequence  uint32
	baseTime  uint64
}

// checkSegmentInvariants walks one media segment and asserts every property a
// player depends on: the box tree is well formed, trun's data_offset lands exactly
// on the first byte of the mdat payload, the sample table matches the access units
// that were written, and the fragment sits at the right point on the timeline.
func checkSegmentInvariants(t *testing.T, seg []byte, want segmentExpectation) {
	t.Helper()

	// Walk the top level, checking every declared size stays inside the buffer.
	var moofStart, moofLen, mdatStart, mdatLen int
	seen := map[string]bool{}
	for off := 0; off < len(seg); {
		if off+8 > len(seg) {
			t.Fatalf("trailing %d bytes are too short for a box header", len(seg)-off)
		}
		size := int(binary.BigEndian.Uint32(seg[off:]))
		typ := string(seg[off+4 : off+8])
		if size < 8 || off+size > len(seg) {
			t.Fatalf("box %q at %d declares size %d, past the %d-byte segment", typ, off, size, len(seg))
		}
		seen[typ] = true
		switch typ {
		case typeMoof:
			moofStart, moofLen = off, size
		case typeMdat:
			mdatStart, mdatLen = off, size
		}
		off += size
	}
	for _, typ := range []string{"styp", typeMoof, typeMdat} {
		if !seen[typ] {
			t.Fatalf("segment is missing the %s box", typ)
		}
	}

	moofBody := seg[moofStart+8 : moofStart+moofLen]
	if got := binary.BigEndian.Uint32(findBoxBody(t, moofBody, "mfhd")[4:]); got != want.sequence {
		t.Fatalf("mfhd sequence_number = %d, want %d", got, want.sequence)
	}

	trafBody := findBoxBody(t, moofBody, "traf")
	tfdt := findBoxBody(t, trafBody, "tfdt")
	if version := tfdt[0]; version != 1 {
		t.Fatalf("tfdt version = %d, want 1", version)
	}
	if got := binary.BigEndian.Uint64(tfdt[4:]); got != want.baseTime {
		t.Fatalf("tfdt baseMediaDecodeTime = %d, want %d", got, want.baseTime)
	}

	tfhd := findBoxBody(t, trafBody, "tfhd")
	tfhdFlags := uint32(tfhd[1])<<16 | uint32(tfhd[2])<<8 | uint32(tfhd[3])
	trun := findBoxBody(t, trafBody, "trun")
	trunFlags := uint32(trun[1])<<16 | uint32(trun[2])<<8 | uint32(trun[3])

	sampleCount := binary.BigEndian.Uint32(trun[4:])
	if int(sampleCount) != len(want.sizes) {
		t.Fatalf("trun sample_count = %d, want %d", sampleCount, len(want.sizes))
	}
	dataOffset := int(int32(binary.BigEndian.Uint32(trun[8:])))

	// Durations live in exactly one of two places, never both and never neither.
	uniform := true
	for _, d := range want.durations {
		if d != want.durations[0] {
			uniform = false
			break
		}
	}
	hasDefault := tfhdFlags&0x000008 != 0
	hasPerSample := trunFlags&0x000100 != 0
	if hasDefault == hasPerSample {
		t.Fatalf("durations must come from tfhd or trun, not both or neither (tfhd flags %#06x, trun flags %#06x)",
			tfhdFlags, trunFlags)
	}
	if hasDefault != uniform {
		t.Fatalf("tfhd default duration present = %v, but the durations are uniform = %v", hasDefault, uniform)
	}

	// Walk the per-sample records and check the sample table against what was
	// written, in order. A reversed or shifted table passes a total-length check
	// but is silently wrong.
	rec := 12
	stride := 4
	if hasPerSample {
		stride = 8
	}
	for i := range want.sizes {
		off := rec + stride*i
		if hasPerSample {
			if got := binary.BigEndian.Uint32(trun[off:]); got != want.durations[i] {
				t.Fatalf("trun sample %d duration = %d, want %d", i, got, want.durations[i])
			}
			off += 4
		}
		if got := binary.BigEndian.Uint32(trun[off:]); got != want.sizes[i] {
			t.Fatalf("trun sample %d size = %d, want %d", i, got, want.sizes[i])
		}
	}
	if uniform {
		if got := binary.BigEndian.Uint32(tfhd[8:]); got != want.durations[0] {
			t.Fatalf("tfhd default_sample_duration = %d, want %d", got, want.durations[0])
		}
	}

	// The offset counts from the start of moof and must land on the mdat payload.
	if got, wantStart := moofStart+dataOffset, mdatStart+8; got != wantStart {
		t.Fatalf("trun data_offset %d resolves to %d, want the mdat payload at %d",
			dataOffset, got, wantStart)
	}

	// The payload must hold exactly the samples that were buffered.
	var wantTotal int
	for _, s := range want.sizes {
		wantTotal += int(s)
	}
	if got := mdatLen - 8; got != wantTotal {
		t.Fatalf("mdat payload is %d bytes, want %d", got, wantTotal)
	}
}

func findBoxBody(t *testing.T, buf []byte, typ string) []byte {
	t.Helper()
	for off := 0; off+8 <= len(buf); {
		size := int(binary.BigEndian.Uint32(buf[off:]))
		if size < 8 || off+size > len(buf) {
			t.Fatalf("malformed box while looking for %q at offset %d", typ, off)
		}
		if string(buf[off+4:off+8]) == typ {
			return buf[off+8 : off+size]
		}
		off += size
	}
	t.Fatalf("box %q not found", typ)
	return nil
}

// FuzzFragmentWriter drives the segment writer with arbitrary access-unit sizes
// and durations, over two consecutive segments so the timeline is exercised as
// well as the framing. The shaped inputs stay inside the segment caps, so every
// write is expected to succeed: the contract asserted is that the writer produces
// a structurally sound segment whose sample table, decode time and sequence number
// all match what was written, and a refusal is a failure rather than a tolerated
// outcome. trun's data_offset gets the
// most attention because it is the one field computed rather than copied, so a
// layout mistake there surfaces as silently wrong output rather than an error.
func FuzzFragmentWriter(f *testing.F) {
	f.Add([]byte{1}, uint32(1024))
	f.Add([]byte{3, 7, 1}, uint32(1024))
	f.Add([]byte{5, 5, 5, 5}, uint32(960))
	f.Add(bytes32(), uint32(1))

	f.Fuzz(func(t *testing.T, lengths []byte, baseDuration uint32) {
		if len(lengths) == 0 || len(lengths) > 256 {
			t.Skip()
		}
		fw, err := NewFragmentWriter(aacFragmentConfig())
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}

		// Exercise Grow on half the inputs: it is a pure capacity hint, so every
		// segment invariant below must hold byte-for-byte whether or not it ran. The
		// hint is deliberately imperfect (an access-unit estimate, not the exact
		// arena size) so the regrow-after-hint path is covered too.
		if len(lengths)%2 == 0 {
			fw.Grow(len(lengths), len(lengths)*8)
		}

		// Two segments from the same input, so the second one starts at a non-zero
		// decode time and carries sequence number 2. A single segment cannot
		// distinguish an accumulated decode time from an assigned one.
		var baseTime uint64
		var buf []byte
		for segment := uint32(1); segment <= 2; segment++ {
			sizes := make([]uint32, 0, len(lengths))
			durations := make([]uint32, 0, len(lengths))
			for i, n := range lengths {
				// Map each byte to a small non-empty access unit, and vary the
				// duration so both the uniform and the per-sample trun paths run.
				auLen := int(n)%64 + 1
				au := make([]byte, auLen)
				for j := range au {
					au[j] = byte(i + j)
				}
				duration := baseDuration
				if i%3 == 1 {
					duration = baseDuration/2 + 1
				}
				if duration == 0 {
					duration = 1
				}
				if err := fw.WriteFrameDuration(au, duration); err != nil {
					t.Fatalf("WriteFrameDuration: %v", err)
				}
				sizes = append(sizes, uint32(auLen))
				durations = append(durations, duration)
			}

			if got := fw.PendingSamples(); got != len(sizes) {
				t.Fatalf("PendingSamples = %d, want %d", got, len(sizes))
			}
			var wantPending uint64
			for _, d := range durations {
				wantPending += uint64(d)
			}
			if got := fw.PendingDuration(); got != wantPending {
				t.Fatalf("PendingDuration = %d, want %d", got, wantPending)
			}

			buf, err = fw.AppendSegment(buf[:0])
			if err != nil {
				t.Fatalf("AppendSegment: %v", err)
			}
			checkSegmentInvariants(t, buf, segmentExpectation{
				sizes:     sizes,
				durations: durations,
				sequence:  segment,
				baseTime:  baseTime,
			})

			baseTime += wantPending
			if got := fw.BaseMediaDecodeTime(); got != baseTime {
				t.Fatalf("BaseMediaDecodeTime after segment %d = %d, want %d", segment, got, baseTime)
			}
		}
	})
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}
