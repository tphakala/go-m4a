// SPDX-License-Identifier: MIT

package box

import (
	"encoding/binary"
	"errors"
	"testing"
)

// The parsers in parse.go are reached in production only through the Reader,
// which has to construct plausible container framing before a sample table is
// ever handed to ParseStsz and friends. FuzzReader therefore exercises the
// entry-count-versus-payload-length bounds checks far less densely than a direct
// target does, because most of its budget goes into building the box tree rather
// than into the table body. These targets feed the parser bodies directly, so
// the classic MP4 table-expansion defence (an entry_count that would make() a
// slice the payload cannot possibly hold) is hit head-on.
//
// The contract every parser owes, over any bytes at all, is the same:
//
//   - it never panics;
//   - a rejection is the package's own errParse sentinel, never a bare error;
//   - a success never allocates a table larger than the payload can back, which
//     is what stops a 32-bit entry_count from driving a multi-gigabyte make.

// seedTableBody returns a version/flags word followed by a big-endian entry
// count and count entries of entrySize bytes each, the shape the stsc/stco/co64
// and stts bodies share. It is used to seed the fuzzers with at least one
// well-formed body so the corpus starts from a parse that succeeds.
func seedTableBody(count uint32, entrySize int) []byte {
	b := make([]byte, 8+int(count)*entrySize)
	binary.BigEndian.PutUint32(b[4:], count)
	return b
}

func FuzzParseStsz(f *testing.F) {
	// A constant-size table (sample_size != 0, no list) and an empty per-sample
	// table (sample_size == 0, count 0).
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 5})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(seedTableBody(0, 0)) // 8 bytes, shorter than the 12-byte minimum

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		constantSize, count, sizes, err := ParseStsz(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		// A per-sample list only exists when the constant size is zero; when it
		// does, its length is the declared count and the count is backed by the
		// payload (12 header bytes plus 4 per entry).
		if constantSize == 0 {
			if len(sizes) != int(count) {
				t.Fatalf("stsz: %d sizes for count %d", len(sizes), count)
			}
			if 12+4*int64(len(sizes)) > int64(len(payload)) {
				t.Fatalf("stsz: %d sizes overrun %d payload bytes", len(sizes), len(payload))
			}
		}
	})
}

func FuzzParseStsc(f *testing.F) {
	f.Add(seedTableBody(0, 12))
	f.Add(seedTableBody(1, 12))
	f.Add([]byte{0, 0, 0, 0}) // shorter than the 8-byte minimum

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		entries, err := ParseStsc(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		if 8+12*int64(len(entries)) > int64(len(payload)) {
			t.Fatalf("stsc: %d entries overrun %d payload bytes", len(entries), len(payload))
		}
	})
}

func FuzzParseStco(f *testing.F) {
	f.Add(seedTableBody(0, 4))
	f.Add(seedTableBody(3, 4))
	f.Add([]byte{0, 0, 0})

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		offsets, err := ParseStco(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		if 8+4*int64(len(offsets)) > int64(len(payload)) {
			t.Fatalf("stco: %d offsets overrun %d payload bytes", len(offsets), len(payload))
		}
	})
}

func FuzzParseCo64(f *testing.F) {
	f.Add(seedTableBody(0, 8))
	f.Add(seedTableBody(2, 8))
	f.Add([]byte{0, 0, 0})

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		offsets, err := ParseCo64(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		if 8+8*int64(len(offsets)) > int64(len(payload)) {
			t.Fatalf("co64: %d offsets overrun %d payload bytes", len(offsets), len(payload))
		}
	})
}

func FuzzParseStts(f *testing.F) {
	f.Add(seedTableBody(0, 8))
	f.Add(seedTableBody(1, 8))
	f.Add([]byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		if _, _, err := ParseStts(payload); err != nil {
			mustParseSentinel(t, err)
		}
	})
}

func FuzzParseDfla(f *testing.F) {
	// A well-formed dfLa: version/flags, then a last STREAMINFO block header
	// (type 0, length 34) and a 34-byte STREAMINFO body.
	good := make([]byte, 0, 8+streamInfoBodyLen)
	// version/flags, then the last-block flag, block type 0, and 24-bit length 34.
	good = append(good, 0, 0, 0, 0, 0x80, 0x00, 0x00, 0x22)
	good = append(good, make([]byte, streamInfoBodyLen)...)
	f.Add(good)
	f.Add([]byte{0, 0, 0, 0}) // too short for a block header

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		streamInfo, err := ParseDfla(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		if len(streamInfo) != streamInfoBodyLen {
			t.Fatalf("dfLa: STREAMINFO length %d, want %d", len(streamInfo), streamInfoBodyLen)
		}
	})
}

func FuzzParseDops(f *testing.F) {
	f.Add([]byte{0, 2, 0, 0, 0, 0, 0xBB, 0x80, 0, 0, 0}) // version 0, 2ch, 48000 Hz
	f.Add([]byte{1, 2, 0, 0, 0, 0, 0xBB, 0x80, 0, 0, 0}) // unsupported version
	f.Add([]byte{0, 1, 0, 0})                            // shorter than the 11-byte minimum

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		if _, _, _, err := ParseDops(payload); err != nil {
			mustParseSentinel(t, err)
		}
	})
}

func FuzzParseTfhd(f *testing.F) {
	f.Add(body(AppendTfhd(nil, 1, 0)))
	f.Add(body(AppendTfhd(nil, 1, 1024)))
	f.Add([]byte{0, 0, 0}) // shorter than the 8-byte minimum

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		tf, err := ParseTfhd(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		// On a successful parse (payload was at least 8 bytes) the decoded Has*
		// flags and DefaultBaseIsMoof must agree with the raw flag bits, so a
		// gated field is never read without its flag set or skipped with it set.
		flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
		if tf.HasBaseDataOffset != (flags&tfhdBaseDataOffsetPresent != 0) ||
			tf.HasDefaultSampleDuration != (flags&tfhdDefaultSampleDurationPresent != 0) ||
			tf.HasDefaultSampleSize != (flags&tfhdDefaultSampleSizePresent != 0) ||
			tf.HasDefaultSampleFlags != (flags&tfhdDefaultSampleFlagsPresent != 0) ||
			tf.DefaultBaseIsMoof != (flags&tfhdDefaultBaseIsMoof != 0) {
			t.Fatalf("tfhd Has* flags disagree with raw flags %#x: %+v", flags, tf)
		}
	})
}

func FuzzParseTrun(f *testing.F) {
	f.Add(body(AppendTrun(nil, 8, []uint32{10, 20, 30}, nil)))
	f.Add(body(AppendTrun(nil, 8, []uint32{10, 20}, []uint32{960, 960})))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 5}) // huge sample_count, no records
	// A body padded past its records: the records must be sliced to the sample
	// count, not to the end of the payload, or an accessor reads the padding.
	f.Add([]byte{0, 0, 2, 0, 0, 0, 0, 1, 0xDE, 0xAD, 0xBE, 0xEF, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, payload []byte) {
		defer noPanic(t, payload)
		tn, err := ParseTrun(payload)
		if err != nil {
			mustParseSentinel(t, err)
			return
		}
		// The retained records must be exactly SampleCount whole records taken from
		// within the payload, since that length is what makes every accessor index
		// safe.
		if want := tn.recordWidth * int(tn.SampleCount); len(tn.records) != want {
			t.Fatalf("trun: %d record bytes for sample_count %d at width %d, want %d",
				len(tn.records), tn.SampleCount, tn.recordWidth, want)
		}
		// Then read every sample through every accessor. This proves the BOUND, not
		// the layout: a wrong offset that stays inside a record reads a neighbouring
		// field and cannot be seen from here, so the field order is pinned by
		// TestParseTrunAllFieldsPresent instead. What this catches is an index that
		// leaves the records at all, which is a panic and which noPanic reports with
		// the input that produced it.
		samples := int(tn.SampleCount)
		if tn.recordWidth == 0 {
			// A run with no per-sample fields may legally declare a sample_count in
			// the billions, because the tfhd/trex defaults describe every sample and
			// no records back them. Looping over that would hang the fuzzer. Every
			// accessor is independent of i on this path (all four offsets are the
			// absent-field sentinel), so one read covers it. With records present,
			// sample_count is bounded by the payload, so the full walk is bounded by
			// the input.
			samples = min(samples, 1)
		}
		for i := range samples {
			_ = tn.SampleDuration(i)
			_ = tn.SampleSize(i)
			_ = tn.SampleFlags(i)
			_ = tn.SampleCompositionOffset(i)
		}
	})
}

// noPanic turns a parser panic into a test failure that names the input, so a
// crasher lands in testdata/fuzz with the bytes that produced it.
func noPanic(t *testing.T, payload []byte) {
	t.Helper()
	if p := recover(); p != nil {
		t.Fatalf("panic on %d-byte input % x: %v", len(payload), payload, p)
	}
}

// mustParseSentinel fails unless err is the package's errParse base error. A
// parser that rejects malformed input with any other error class has broken the
// contract the Reader relies on to map rejections onto ErrCorrupt.
func mustParseSentinel(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errParse) {
		t.Fatalf("error %v is not errParse", err)
	}
}
