# lith

**L**inear writes, **I**mmediate mode, **T**icks, **H**ypermedia: the shared
runtime for the Go, SQLite and Datastar projects of zwei und eins.

- **Linear writes:** one writer applies every command in order, batched into
  one transaction with a savepoint each.
- **Immediate mode:** a page is `view = f(state)`, rendered whole and morphed
  by Datastar; no client state predicts the server.
- **Ticks:** commits mark views dirty, and each changed view renders once per
  tick for everyone who watches it.
- **Hypermedia:** frames go out over SSE, each stream compressed on its own.

The name nods to Anders Murphy's [hyperlith](https://github.com/andersmurphy/hyperlith),
whose design it follows in Go.

See doc.go for the package map and skills/lith for how to use it.
A new project starts from template/: `go run ./cmd/lith-new -h`.
