// SPDX-License-Identifier: MIT

package aacm4a_test

import (
	"io"
	"log"
	"math"
	"os"
	"time"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/aacm4a"
)

// ExampleNewSeekableDecoder reads a three second window that starts at ten
// seconds, without decoding the first ten.
func ExampleNewSeekableDecoder() {
	f, err := os.Open("recording.m4a")
	if err != nil {
		log.Fatal(err)
	}

	d, info, err := aacm4a.NewSeekableDecoder(f, m4a.WithMaxBoxBuffer(1<<20))
	if err != nil {
		_ = f.Close()
		log.Fatal(err)
	}
	// The output starts exactly at the requested time; the decoder decodes and
	// drops the codec pre-roll itself.
	if err := d.SeekTime(10 * time.Second); err != nil {
		_ = f.Close()
		log.Fatal(err)
	}
	window := int64(math.Round(3 * float64(info.SampleRate)))
	stride := int64(info.Channels) * 2
	_, err = io.CopyN(io.Discard, d, window*stride)
	_ = f.Close()
	if err != nil {
		log.Fatal(err)
	}
}
