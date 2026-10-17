module github.com/tphakala/go-m4a

go 1.27.0

require (
	// Ruleguard DSL backs the custom gocritic ruleguard matchers in rules/*.go.
	// Those files carry the `ruleguard` build tag, so the normal toolchain never
	// compiles them; `go mod tidy` still keeps the requirement because tidy
	// considers every build tag.
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	github.com/tphakala/go-aac v0.7.1
	github.com/tphakala/go-flac v1.3.0
	github.com/tphakala/go-opus v1.2.1
)

require (
	github.com/tphakala/simd v1.13.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)
