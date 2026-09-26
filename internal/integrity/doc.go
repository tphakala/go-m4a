// SPDX-License-Identifier: MIT

// Package integrity is the container round-trip integrity gate. It holds no
// library code: its tests encode a small deterministic corpus through each codec
// bridge (aacm4a, opusm4a, flacm4a), decode it back, and check what the container
// is responsible for rather than what the inner codec is. That means the box
// layout, an edit list whose priming trim and presented length match the source
// exactly, a decode that is sample-accurate once the edit list is applied, and a
// bit-exact round trip for FLAC. Perceptual quality belongs to the codec repos.
//
// The integer facts each case produces are pinned in testdata/baseline.json, so
// a change in container behaviour fails the suite even when it still passes the
// structural checks. Regenerate the baseline after an intended change with
//
//	go test ./internal/integrity -run TestContainerIntegrity -update
//
// and review the diff. When ffmpeg is on PATH the tests also cross-check the
// container against it in both directions; setting M4A_REQUIRE_FFMPEG=1 turns
// its absence into a failure, which is how CI keeps those checks from skipping
// silently.
package integrity
