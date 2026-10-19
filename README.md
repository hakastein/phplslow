# phplslow

[![CI](https://github.com/hakastein/phplslow/actions/workflows/ci.yml/badge.svg)](https://github.com/hakastein/phplslow/actions/workflows/ci.yml)

phplslow is a PHP language server written in Go. A client starts it and speaks the
[Language Server Protocol](https://microsoft.github.io/language-server-protocol/) over stdin and stdout. phplslow
indexes the PHP files of the workspace into SQLite on disk and answers from that index.

## What it answers

- `textDocument/definition` and `textDocument/references` for a method call whose receiver class is known from the
  code around it:
  - `(new Cart())->add()` and `(new Cart())?->add()`, anywhere, also at the top level of a file;
  - `$cart->add()` inside a function, method or closure body where every assignment to `$cart` in that body is
    `new Cart(...)` of one class, the first of them comes before the call and nothing can bind `$cart` to another
    value. An arrow function sees the variables of its parent; a closure sees those it imports with `use` by value.
    Such a variable is known when, following the chain of arrow functions and `use` captures outward, it comes
    from a function, method or closure body where it is known; a chain that reaches the top level of the file
    gives an unknown variable.
  - A variable at the top level of a file is never known, also inside an arrow function or a closure that takes it
    from there. A superglobal (`$GLOBALS`, `$_SERVER`, `$_GET`, `$_POST`, `$_FILES`, `$_COOKIE`, `$_SESSION`,
    `$_REQUEST`, `$_ENV`, case-sensitive) is never known in any scope.
  - Any other write makes the variable unknown: another assignment, a compound assignment, `++`/`--`, a parameter,
    `foreach` (also by reference), `list(...)` and `[...]` destructuring (also with keys), `catch`, `global`,
    `static`, `unset`, a reference (`$y = &$x`, `use (&$x)`), and passing it as an argument (the parameter may be
    by reference). A method call on the variable itself does not.
  - Every variable of the body is unknown when the body has `$$name` or `${...}` (also in `global $$name`),
    `extract`, `eval`, `include`, `include_once`, `require`, `require_once`, or `parse_str` or `mb_parse_str` with
    one argument.
  - `new self`, `new static`, `new parent`, an anonymous class and a dynamic class name give no receiver class.
  - These rules are recorded in [ADR 0001](docs/adr/0001-workspace-index-on-disk.md) (in Russian).
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
- Skipped on purpose, and only these: an entry that is not a regular file (a named pipe, a socket, a device), a
  directory already on the path from the root, and a broken symlink whose name does not end in `.php` (it is
  logged to stderr). Any other entry that cannot be read fails the index: a directory that cannot be listed, an
  entry that cannot be stat-ed, a `.php` file that cannot be opened or read, a broken `.php` symlink. Requests then
  get `RequestFailed` with the path and the OS error, also when a ready index exists; once the cause is removed,
  the next start answers in full.
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
