// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"

	m4a "github.com/tphakala/go-m4a"
)

// FuzzSeekableDecoder hands attacker-controlled container bytes and seek targets
// to the seekable decoder. The contract asserted is the bridge's own: no panic,
// no more output after the last seek than the track can decode to, and every
// error is a typed sentinel. Seeds are small go-aac encodes generated here.
func FuzzSeekableDecoder(f *testing.F) {
	for _, ch := range []int{1, 2} {
		fx := encodeFixture(f, 44100, ch, 32000, false, true)
		f.Add(fx.data, 5, int64(300*time.Millisecond))
	}

	f.Fuzz(func(t *testing.T, data []byte, sample int, nanos int64) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic on %d-byte input: %v", len(data), p)
			}
		}()
		typed := func(what string, err error) {
			if err == nil || errors.Is(err, io.EOF) {
				return
			}
			sentinels := []error{
				m4a.ErrCorrupt, m4a.ErrUnsupported, m4a.ErrOutOfRange, m4a.ErrBoxTooLarge,
				aacpcm.ErrCorruptStream, aacpcm.ErrUnsupported,
			}
			for _, s := range sentinels {
				if errors.Is(err, s) {
					return
				}
			}
			t.Fatalf("%s error on %d-byte input is not a typed sentinel: %v", what, len(data), err)
		}

		d, info, err := NewSeekableDecoder(bytes.NewReader(data))
		if err != nil {
			typed("open", err)
			return
		}
		typed("SeekSample", d.SeekSample(sample))
		_, err = io.CopyN(io.Discard, d, 64<<10)
		typed("read", err)
		typed("SeekTime", d.SeekTime(time.Duration(nanos)))
		n, err := io.Copy(io.Discard, d)
		typed("read", err)
		if limit := int64(info.FrameCount) * aac.FrameSize * int64(d.stride); n > limit {
			t.Fatalf("read %d bytes after the last seek, above the track's %d", n, limit)
		}
	})
}
