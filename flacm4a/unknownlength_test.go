// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"encoding/binary"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// clearTotalSamples rewrites the STREAMINFO total-samples field of a FLAC-in-MP4
// buffer to zero, the value FLAC defines as "unknown length". The bridge encoder
// always knows the sample count and writes it, so a stream that declares an
// unknown length cannot be produced through the encoder; it has to be forged from
// one that does. This is the input the decode reservation never sizes from, so it
// is the case where any cost the reservation path adds is pure loss, and it had no
// end-to-end coverage at all until this test (#20).
//
// The 34-byte STREAMINFO body packs sample_rate (20 bits), channels (3), bits per
// sample (5) and total_samples (36) into the eight bytes at offset 10. Clearing
// the low 36 bits zeroes total_samples while leaving the format fields intact. The
// dfLa box lays the STREAMINFO body 12 bytes past the box's four-character name
// (four bytes of name, then the FullBox version/flags and the metadata block
// header the bridge writes ahead of it). The MD5 that follows still matches the
// audio, since the samples are untouched, so a decoder that verifies it stays
// happy.
func clearTotalSamples(tb testing.TB, buf []byte, samplesPerChannel int) {
	tb.Helper()
	// moov is the last top-level box (ftyp, mdat, moov), so search from the end:
	// LastIndex skips the whole mdat audio payload, where the four bytes "dfLa"
	// could otherwise occur by chance in an entropy-coded frame and be matched
	// ahead of the real box.
	idx := bytes.LastIndex(buf, []byte("dfLa"))
	if idx < 0 {
		tb.Fatal("no dfLa box in encoded buffer")
	}
	off := idx + 12 + 10 // dfLa name -> STREAMINFO body -> packed rate/channels/bps/total
	if off+8 > len(buf) {
		tb.Fatalf("STREAMINFO field at %d overruns %d-byte buffer", off, len(buf))
	}
	const totalMask = (uint64(1) << 36) - 1
	packed := binary.BigEndian.Uint64(buf[off:])
	// Positive control: the located field must already hold the exact inter-channel
	// sample count the encoder wrote. If it does not, the search landed somewhere
	// other than STREAMINFO's total_samples, and clearing it would silently leave the
	// real total_samples intact, so the caller would decode down the ordinary
	// reservation path while believing it exercised the unknown-length one. Assert the
	// value so a future STREAMINFO/dfLa framing drift fails loudly rather than turning
	// the unknown-length test and benchmark into no-ops.
	if got := packed & totalMask; got != uint64(samplesPerChannel) {
		tb.Fatalf("located field holds total_samples %d, want %d: dfLa search mis-targeted", got, samplesPerChannel)
	}
	binary.BigEndian.PutUint64(buf[off:], packed&^totalMask) // clear total_samples
}

// TestDecodeUnknownLength covers the TotalSamples=0 decode path end to end: a
// legal FLAC stream that declares an unknown length must still decode to exactly
// its samples, byte-identical, even though the reservation cannot size the output
// buffer from the header.
func TestDecodeUnknownLength(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(map[int]string{1: "mono", 2: "stereo"}[channels], func(t *testing.T) {
			const samplesPerCh = 12000
			pcm := genS16(samplesPerCh, channels)
			var buf memWS
			cfg := Config{SampleRate: 48000, Channels: channels, BitDepth: 16, CompressionLevel: 5}
			if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}
			clearTotalSamples(t, buf.buf, samplesPerCh)

			got, info, err := DecodeInterleaved(bytes.NewReader(buf.buf))
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if info.Codec != m4a.CodecFLAC {
				t.Errorf("Codec = %v, want FLAC", info.Codec)
			}
			if !bytes.Equal(got, pcm) {
				t.Errorf("unknown-length decode mismatch: got %d bytes, want %d", len(got), len(pcm))
			}
		})
	}
}
