# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `Reader.SeekSample` positions the reader at any access unit without reading the ones before it, for plain and fragmented input. It does no I/O itself, so a jump costs one seek on the next read. An index outside the track returns the new `ErrOutOfRange` sentinel.

### Changed

- The minimum Go version is now 1.27.

Releases up to and including v0.5.0 predate this file; see the [GitHub releases](https://github.com/tphakala/go-m4a/releases) and the git tags for their history.

[Unreleased]: https://github.com/tphakala/go-m4a/compare/v0.5.0...HEAD
