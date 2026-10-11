// SPDX-License-Identifier: MIT

package testutil

import (
	"bytes"
	"io"
	"testing"
)

func TestWriteSeekerOverwriteAndGrow(t *testing.T) {
	type op struct {
		seek   bool
		off    int64
		whence int
		data   string
		want   string // Bytes() after the op
	}
	tests := []struct {
		name string
		ops  []op
	}{
		{"append", []op{
			{data: "abc", want: "abc"},
			{data: "de", want: "abcde"},
		}},
		{"seek back and shorter overwrite keeps the tail", []op{
			{data: "abcdef", want: "abcdef"},
			{seek: true, off: 1, whence: io.SeekStart},
			{data: "XY", want: "aXYdef"},
		}},
		{"seek current negative then write", []op{
			{data: "abcd", want: "abcd"},
			{seek: true, off: -2, whence: io.SeekCurrent},
			{data: "ZZ", want: "abZZ"},
		}},
		{"seek end then write appends", []op{
			{data: "ab", want: "ab"},
			{seek: true, off: 0, whence: io.SeekStart},
			{seek: true, off: 0, whence: io.SeekEnd},
			{data: "cd", want: "abcd"},
		}},
		{"seek past the end zero-fills the gap", []op{
			{data: "ab", want: "ab"},
			{seek: true, off: 4, whence: io.SeekStart},
			{data: "c", want: "ab\x00\x00c"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ws WriteSeeker
			for i, o := range tt.ops {
				if o.seek {
					if _, err := ws.Seek(o.off, o.whence); err != nil {
						t.Fatalf("op %d: Seek: %v", i, err)
					}
					continue
				}
				n, err := ws.Write([]byte(o.data))
				if err != nil || n != len(o.data) {
					t.Fatalf("op %d: Write = %d, %v", i, n, err)
				}
				if got := ws.Bytes(); string(got) != o.want {
					t.Fatalf("op %d: Bytes() = %q, want %q", i, got, o.want)
				}
			}
		})
	}
}

func TestWriteSeekerSeekErrors(t *testing.T) {
	var ws WriteSeeker
	if _, err := ws.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Seek(0, 99); err == nil {
		t.Error("bad whence: want an error")
	}
	if _, err := ws.Seek(-5, io.SeekCurrent); err == nil {
		t.Error("negative result: want an error")
	}
	// Both failures must leave the position at 1.
	if _, err := ws.Write([]byte("Z")); err != nil {
		t.Fatal(err)
	}
	if got := ws.Bytes(); string(got) != "aZcd" {
		t.Errorf("Bytes() = %q, want %q (position moved by a failed Seek)", got, "aZcd")
	}
}

func TestWriteSeekerRewindReusesBuffer(t *testing.T) {
	first := bytes.Repeat([]byte{1}, 64)
	second := bytes.Repeat([]byte{2}, 64)
	var ws WriteSeeker
	if _, err := ws.Write(first); err != nil {
		t.Fatal(err)
	}
	ws.Rewind()
	if _, err := ws.Write(second); err != nil {
		t.Fatal(err)
	}
	if got := ws.Bytes(); !bytes.Equal(got, second) {
		t.Fatalf("Bytes() = %v, want the second write", got)
	}
	allocs := testing.AllocsPerRun(10, func() {
		ws.Rewind()
		_, _ = ws.Write(second)
	})
	if allocs != 0 {
		t.Errorf("rewind and rewrite allocated %v times, want 0", allocs)
	}
}
