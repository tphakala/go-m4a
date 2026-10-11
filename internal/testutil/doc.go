// SPDX-License-Identifier: MIT

// Package testutil holds helpers shared by the test files of every package in
// this module: an in-memory io.WriteSeeker, signal helpers for alignment
// checks, and LookTool for locating ffmpeg and ffprobe.
//
// It is imported only from _test.go files, because it imports testing; the
// depguard rule in .golangci.yaml enforces that. It is stdlib-only and must
// never import the root package or a bridge: every root test file is
// package m4a, so that import would be a cycle.
package testutil
