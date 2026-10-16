# app: notes for coding agents

app is a Go, SQLite and Datastar application on lith (github.com/zweiundeins/lith), started from lith's template. Read the lith skill before changing code. Its rules shape every change here (one writer, commands that touch topics, views rendered once per tick for all tabs, outcomes on the stream, tab state in SQLite), and its references hold the recipes, the review checklist and the Datastar gotchas. The skill ships with lith, so it matches the lith version in go.mod:

    $(go list -m -f '{{.Dir}}' github.com/zweiundeins/lith)/skills/lith/SKILL.md

## Layout

- `cmd/app`: the binary (`serve`, `backup`, `--version`) and its wiring: database, writer, render loop, the edge, shutdown, the hourly sweep. Every flag can also come from the environment (`-db` as `APP_DB`).
- `internal/guestbook`: the template's example feature. Commands (`Sign`, `Sweep`) in `commands.go`, queries in `queries.go`. Replace it with the first real feature.
- `internal/views`: templ components, and the Datastar expressions they use (`expr.go`).
- `internal/web`: routes, handlers, the live views, static files. `static/vendor` holds Starbase's patched Datastar build; its `VERSION` says which. lith's `newest-frame.js` (`live.NewestFrame`) is served beside them as `/static/lith/newest-frame.<hash>.js`, and the page loads it after Datastar.
- `migrations`: `NNN_name.sql`, applied in order at startup. Add a file for every schema change, never edit one that has shipped.
- `deploy`: systemd units, the Caddy site block, deploy script, backups, and in `deploy/observability` the probes and log shipping a host's Prometheus and Alloy load (`deploy/README.md`).
- `benchmarks/RESULTS.md`: measured numbers, each with its date, machine and command.

## Commands

- `go tool task dev`: serve on http://127.0.0.1:8080 and rebuild on every change.
- `go tool task test`: vet, gofmt, `go test -race ./...`.
- `go tool task build`: the static binary.
- `go tool templ generate` after editing a `.templ` file. The `_templ.go` files are committed.

## Project rules

Rules that hold for this project only go here.
