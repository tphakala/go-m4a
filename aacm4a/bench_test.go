// SPDX-License-Identifier: MIT

package aacm4a

import (
	"testing"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// benchADTSFrames is the frame count for the de-ADTS bridge benchmark, matching
// a realistic short clip.
const benchADTSFrames = 5000

// buildADTS assembles a synthetic CRC-less ADTS stream of n frames with realistic
// payload sizes (100..800 bytes). Each frame is a 7-byte header (syncword,
// protection_absent set, frame_length encoded) followed by the raw access unit,
// exactly the shape go-aac emits and writeADTSFrames consumes. No go-aac encode
// is involved, so this isolates the container-side de-ADTS split cost.
func buildADTS(n int) []byte {
	var buf []byte
	var hdr [adtsHeaderLen]byte
	for i := 0; i < n; i++ {
		payloadLen := 100 + (i*97)%701
		frameLen := adtsHeaderLen + payloadLen
		hdr[0] = 0xff
		hdr[1] = 0xf1 // 1111 0001: syncword tail, layer 00, protection_absent 1
		hdr[2] = 0x00
		hdr[3] = byte((frameLen >> 11) & 0x03)
		hdr[4] = byte((frameLen >> 3) & 0xff)
		hdr[5] = byte((frameLen & 0x07) << 5)
		hdr[6] = 0x00
		buf = append(buf, hdr[:]...)
		payload := make([]byte, payloadLen)
		for j := range payload {
			payload[j] = byte(i + j)
		}
		buf = append(buf, payload...)
	}
	return buf
}

// BenchmarkWriteADTSFrames measures the de-ADTS split loop plus per-frame
// WriteFrame into an in-memory writer, without invoking the go-aac encoder. The
// backing buffer is reused across iterations so the numbers isolate the bridge
// and Writer allocations from the sink's growth.
func BenchmarkWriteADTSFrames(b *testing.B) {
	adts := buildADTS(benchADTSFrames)
	cfg := m4a.WriterConfig{SampleRate: 48000, Channels: 1, ASC: []byte{0x11, 0x88}}

	ws := &memWriteSeeker{}
	// Warm-up mux grows the in-memory sink to the final file size so the measured
	// iterations reuse the backing buffer and isolate the bridge plus Writer cost
	// from the sink's O(N^2) growth. b.Loop excludes this from the counters.
	muxADTSOnce(b, ws, cfg, adts)

	b.SetBytes(int64(len(adts)))
	b.ReportAllocs()
	for b.Loop() {
		ws.pos = 0
		muxADTSOnce(b, ws, cfg, adts)
	}
}

// muxADTSOnce de-ADTS-splits adts into a fresh Writer over ws and closes it.
func muxADTSOnce(b *testing.B, ws *memWriteSeeker, cfg m4a.WriterConfig, adts []byte) {
	b.Helper()
	wr, err := m4a.NewWriter(ws, cfg)
	if err != nil {
		b.Fatal(err)
	}
	if err := writeADTSFrames(wr, adts); err != nil {
		b.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkEncodeInterleaved measures the whole PCM-to-m4a path for the clip
// shape issue #8 profiled: 15 s mono 48 kHz at 96 kbps. The interesting number is
// B/op, which the ADTS buffer reservation reduces by removing the growth chain.
func BenchmarkEncodeInterleaved(b *testing.B) {
	const sampleRate, channels, seconds = 48000, 1, 15
	cfg := aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Bitrate: 96000}
	pcm := chirpS16(sampleRate*seconds, channels, sampleRate)
	b.ReportAllocs()
	b.SetBytes(int64(len(pcm)))
	for b.Loop() {
		var ws memWriteSeeker
		if err := EncodeInterleaved(&ws, cfg, pcm); err != nil {
			b.Fatalf("EncodeInterleaved: %v", err)
		}
	}
}
