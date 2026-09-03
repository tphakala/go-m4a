// SPDX-License-Identifier: MIT

package box

import (
	"bytes"
	"errors"
	"testing"
)

// childBox builds one box (8-byte header + body) for a FindChild fixture.
func childBox(typ string, body []byte) []byte {
	out := AppendBoxHeader(nil, uint32(8+len(body)), NewFourCC(typ))
	return append(out, body...)
}

func TestFindChildFirstMatch(t *testing.T) {
	t.Parallel()
	// Two "aaaa" children frame the run; FindChild must return the first.
	payload := bytes.Join([][]byte{
		childBox("aaaa", []byte{1, 2, 3}),
		childBox("bbbb", []byte{4, 5}),
		childBox("aaaa", []byte{9}),
	}, nil)

	body, found, err := FindChild(payload, NewFourCC("aaaa"))
	if err != nil {
		t.Fatalf("FindChild: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if !bytes.Equal(body, []byte{1, 2, 3}) {
		t.Errorf("body = %v, want the first aaaa child [1 2 3]", body)
	}

	body, found, err = FindChild(payload, NewFourCC("bbbb"))
	if err != nil || !found || !bytes.Equal(body, []byte{4, 5}) {
		t.Errorf("FindChild(bbbb) = %v, %v, %v; want [4 5], true, nil", body, found, err)
	}
}

func TestFindChildMissing(t *testing.T) {
	t.Parallel()
	payload := childBox("aaaa", []byte{1})
	body, found, err := FindChild(payload, NewFourCC("zzzz"))
	if err != nil {
		t.Fatalf("FindChild: %v", err)
	}
	if found {
		t.Errorf("found = true, want false")
	}
	if body != nil {
		t.Errorf("body = %v, want nil", body)
	}
}

// TestFindChildFramingCheckedAfterMatch pins the full-walk contract: a malformed box
// following the match is still reported, so a caller that skips the container's body
// (a foreign traf) does not skip its framing validation.
func TestFindChildFramingCheckedAfterMatch(t *testing.T) {
	t.Parallel()
	// A valid match, then a header declaring a size that runs past the parent.
	bad := AppendBoxHeader(nil, 999, NewFourCC("cccc")) // 8-byte header, no 991 body bytes
	payload := append(childBox("aaaa", []byte{1}), bad...)

	_, _, err := FindChild(payload, NewFourCC("aaaa"))
	if err == nil {
		t.Fatal("FindChild returned nil error despite a malformed trailing box")
	}
	if !errors.Is(err, errParse) {
		t.Errorf("error = %v, want it to wrap errParse", err)
	}
}
