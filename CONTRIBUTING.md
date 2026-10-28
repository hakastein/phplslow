# Contributing

Bug reports and pull requests are welcome on [GitHub](https://github.com/hakastein/phplslow).

## Before you start

- phplslow is a standalone LSP server. It follows the LSP specification and its own contract; behaviour is not
  shaped for a particular client.
- Open an issue before a change of behaviour. A change of a recorded decision updates its ADR in the same pull
  request.
- Design records ([`docs/adr/`](docs/adr/)) and the glossary ([`CONTEXT.md`](CONTEXT.md)) are in Russian.
- A bug found on a private code base comes as a small self-written PHP fixture, never as that code.

## Development

```bash
make go      # gofmt check, go vet, go test
make build   # bin/phplslow
```

Everything builds with `CGO_ENABLED=0`. CI runs the same targets on every push and pull request.

## Tests

- The seam is the phplslow process: `TestMain` builds the binary, a test starts it, speaks LSP to it over stdio and
  checks the answers. Tests do not read the SQLite tables of the index.
- Every test gets its own cache directory, so indexes of tests never meet.
- Fixtures are small PHP files in `testdata/`; a test finds a place in a file by a unique substring.
- Tests live in the external package `main_test` and use testify with table cases; a test body has no logic.
- Lines phplslow writes to stderr are diagnostics, not a protocol. A test reads them only where nothing else shows
  the behaviour.

## Pull requests

- One change per pull request, against `main`.
- Code borrowed under MIT keeps its authors' notice.
- `make go` passes.
