# Contributing

Thanks for your interest. Bug reports and pull requests are welcome; for anything larger than a small fix, open an issue first so the approach can be agreed before the work is done.

## Before you open a pull request

Run the full local gate and make sure it is green:

```sh
task check
```

That is build, vet (amd64, arm64, and linux/386), `go mod tidy -diff`, the linter (with the config verified first), the ruleguard probe test, and the tests with the race detector. CI runs the same steps with the same pinned linter version, so a green `task check` is a green CI in practice.

Install the pinned tools with `task tools` if you do not have them.

## Guidelines

- Keep pull requests focused; one change per pull request.
- Add tests for new behaviour and for every bug fix. Before you push, break the line a new test pins and confirm the test fails; a test that cannot fail proves nothing.
- The library parses untrusted bytes, so cover new parsing paths with a fuzz target or a seed under `testdata/fuzz` where it makes sense.
- Do not add a `//nolint` directive without a linter name and a reason. If a linter is wrong often enough to be worth silencing globally, change `.golangci.yaml` with a comment saying why.
- New ruleguard matchers need a probe section (see the file comment in `rules/testdata/probe/probe.go`); `task rules` checks it.
- Note user-facing changes in `CHANGELOG.md` under **Unreleased**.
- Write commit messages in the imperative with a type prefix (`feat:`, `fix:`, `docs:`, `build:`, `ci:`, `chore:`, `refactor:`, `test:`), optionally scoped (`fix(reader): ...`).

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open a public issue for a vulnerability.
