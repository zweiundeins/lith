---
title: Getting started
nav: Start a project
summary: Make a project from lith's template, run it, and find your way around it.
order: 1
---

## What you need

Go 1.27 or later: lith uses generic methods (`DB.ViewValue`, `Writer.DoValue`), which came with Go 1.27. SQLite comes as pure Go ([modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)), so no C compiler is involved and every binary builds with `CGO_ENABLED=0`. A project's tools, templ, air and task, run through `go tool` from its go.mod.

## Make a project

lith-new copies lith's template into a new module:

```sh
go run github.com/zweiundeins/lith/cmd/lith-new@latest ~/develop/guestbook example.com/acme/guestbook
cd ~/develop/guestbook && go mod tidy && go tool task test
```

The app takes its name from the module path's last element, `guestbook` here, or from `-name`. Its binary sits in `cmd/guestbook`, its systemd units in `deploy/`, and every flag can also come from the environment: `-db` from `GUESTBOOK_DB`. With `-lith <dir>`, the new go.mod gets a `replace` directive for a lith checkout, for work on both at once.

## Run it

```sh
go tool task dev
```

The server listens on http://127.0.0.1:8080 and rebuilds on every change to Go, templ, CSS, JS and SQL files. Open the page in two windows and sign the guestbook in one. The entry appears in both within a tick, and only the window that signed says "Thank you for signing." Sign the same words again, and only that window hears why nothing changed.

## What the template holds

The template is a guestbook that uses each rule once: a command, a shared view, a view of the tab's own, and a button that stays pending until its outcome arrives on the stream.

| Path | Holds |
| --- | --- |
| `cmd/guestbook` | the binary: `serve` (the default), `backup` and `--version`, and the wiring |
| `internal/guestbook` | the example feature: commands in `commands.go`, queries in `queries.go` |
| `internal/views` | templ components, and the Datastar expressions they use |
| `internal/web` | routes, handlers, the live views, the static files and the vendored Datastar build |
| `migrations` | `NNN_name.sql` files, applied in order at startup |
| `deploy` | a systemd unit, a Caddy block, a deploy script with a health check and rollback, a backup timer |

`go tool task test` vets the code, checks its formatting and runs the tests with the race detector. `go tool task build` builds the static binary. The template's `AGENTS.md` points coding agents to lith's skill in `skills/lith`, which ships with lith and so matches the version in go.mod.

[Wiring](/docs/getting-started/wiring) goes through `main` part by part, and [The first command and view](/docs/getting-started/first-view) adds a feature next to the guestbook.
