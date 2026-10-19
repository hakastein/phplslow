# phplslow

[![CI](https://github.com/hakastein/phplslow/actions/workflows/ci.yml/badge.svg)](https://github.com/hakastein/phplslow/actions/workflows/ci.yml)

phplslow is a PHP language server written in Go. A client starts it and speaks the
[Language Server Protocol](https://microsoft.github.io/language-server-protocol/) over stdin and stdout. phplslow
indexes the PHP files of the workspace into SQLite on disk and answers from that index.

## What it answers

- `textDocument/definition` and `textDocument/references` for a method call whose receiver class is known from the
  code around it:
  - `(new Cart())->add()` and `(new Cart())?->add()`;
  - `$cart->add()` where every assignment to `$cart` in the same function, method, closure or file top level is
    `new Cart(...)` of one class, the first of them comes before the call and nothing can bind `$cart` to another
    value. An arrow function sees the variables of its parent; a closure sees those it imports with `use` by value.
  - Passing `$cart` as an argument (the parameter may be by reference), `$$name`, `extract`, `eval`, `include`/
    `require` and one-argument `parse_str` make it unknown. A top-level variable is unknown when any file of the
    workspace uses `global $cart` or `$GLOBALS['cart']`, or `$GLOBALS` with a key that is not a string literal.
- The answer lists the method declared in that class. A call becomes a reference only when that class declares the
  method; a call that is not resolved this way is not a reference, whatever its method name.
- `references` from a call site or from the method declaration lists every resolved call site of the method;
  `context.includeDeclaration` adds the declaration.
- Class and method names compare case-insensitively, as in PHP.

Nothing else is resolved: no inheritance, interfaces, traits, `$this`, `self`, `static`, parameters, properties,
return types or PHPDoc. Other requests are answered with `MethodNotFound`.

## The index

- A request sent before the index is ready waits for it and gets the full answer. `$/cancelRequest` ends a waiting
  request with `RequestCancelled`. If the index cannot be opened or built, requests fail with `RequestFailed`.
- Symbolic links to files and directories are followed, also outside the root; a file is named by its path through
  the link, and a directory already on the path from the root is not walked again. Only regular files are read.
- phplslow chooses where the index lives: one SQLite file per workspace root under the user's cache directory
  (`os.UserCacheDir()`, e.g. `$XDG_CACHE_HOME` or `~/.cache` on Linux) in `phplslow/`.
- At start phplslow compares the files under the root (path and content hash of every `.php` file) with the ones the
  index was built from. If they are the same, the index is used as it is; otherwise it is built again as a whole.
- Answers come from the files as they were when the index was built; edits made while the server runs are not seen
  until the next start.

## Build

Requires Go 1.26 or later. The build does not use CGO.

```bash
make build   # bin/phplslow
```

Point your LSP client at `bin/phplslow` with no arguments and a `file://` `rootUri` (or a workspace folder) in
`initialize`.

## Documentation

- Decisions: [docs/adr/](docs/adr/) (in Russian).
- Glossary: [CONTEXT.md](CONTEXT.md) (in Russian).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
