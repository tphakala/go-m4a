// SPDX-License-Identifier: MIT

// Package testutil holds helpers shared by the test files of the root package,
// the codec bridges and internal/integrity: an in-memory io.WriteSeeker,
// MDATPayload for locating the raw access units in an encoded file, signal
// helpers for alignment checks, and LookTool for locating ffmpeg and ffprobe.
//
// It is imported only from _test.go files, because it imports testing; the
// depguard rule in .golangci.yaml enforces that. It is stdlib-only and must
// never import the root package or a bridge: every root test file is
// package m4a, so that import would be a cycle.
package testutil
