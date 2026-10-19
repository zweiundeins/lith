# app: notes for coding agents

app is a Go, SQLite and Datastar application on lith (github.com/zweiundeins/lith), started from lith's template. Read the lith skill before changing code. Its rules shape every change here (one writer, commands that touch topics, views rendered once per tick for all tabs, outcomes on the stream, tab state in SQLite), and its references hold the recipes, the review checklist and the Datastar gotchas. The skill ships with lith, so it matches the lith version in go.mod:

    $(go list -m -f '{{.Dir}}' github.com/zweiundeins/lith)/skills/lith/SKILL.md

## Layout

- `cmd/app`: the binary (`serve`, `backup`, `--version`) and its wiring: database, writer, render loop, the edge, shutdown, the hourly sweep. Every flag can also come from the environment (`-db` as `APP_DB`). `metrics.go` serves the metrics for Prometheus on a loopback listener of their own, never the public one: `lith/prom` and `lith/dbmetrics/prom` under the namespace `app_`, the Go runtime and the process. `-metrics` sets the address: `auto` (the default) is 127.0.0.1 at `-addr`'s port + 100, `off` is none. `TestDashboard` checks the names against the dashboard and alert rules in lith's `deploy/observability`.
- `internal/guestbook`: the template's example feature. Commands (`Sign`, `Sweep`) in `commands.go`, queries in `queries.go`. Replace it with the first real feature.
- `internal/views`: templ components, and the Datastar expressions they use (`expr.go`).
- `internal/web`: routes, handlers, the live views, static files. `static/vendor` holds Starbase's patched Datastar build; its `VERSION` says which. lith's `newest-frame.js` (`live.NewestFrame`, vendored from github.com/mbolli/datastar-attribute-newest-frame) is served beside them as `/static/lith/newest-frame.<hash>.js`, and the page loads it after Datastar, with `data-newest-frame` on its body. It applies the newest frame of each view, and when the page falls behind its stream, it dispatches `newest-frame-behind`, which `<body>` posts to `POST /pace` (`pace.go`). The handler counts the report with `live.Hub.Behind` (`app_live_newest_frame_behind_total`) and paces that stream with `live.Hub.Pace`: the hub then sends the page a write only once it has acknowledged the last. The stream's `live.Spec.Ack` ends each paced write with an element that posts `POST /ack` after the paint, with the page's round trip, and the hub sends every frame again once the page keeps up for 5 s. Streams are named after the session and the tab (`streamID`), so `/ack` and `/pace` reach only the request's own streams; `/pace` is limited per session and `/ack` per stream. A page that keeps up never posts either. Paced, a slow phone gets a third of the bytes; on a far link it gets one frame per round trip, so it lags a little more than with the plugin alone (lith's streams guide has the numbers). Without `Ack` in the stream's `Spec`, `/pace` only counts. Scripts and stylesheets go out minified (`edge.Minify(minify.Bytes)`, from `github.com/zweiundeins/lith/minify`), and as written with `-dev`.
- `migrations`: `NNN_name.sql`, applied in order at startup. Add a file for every schema change, never edit one that has shipped.
- `deploy`: systemd units, the Caddy site block, deploy script, backups, and in `deploy/observability` the metrics target, the probes and the log shipping a host's Prometheus and Alloy load (`deploy/README.md`). The metrics port, 8180, is in three places: the unit's `-metrics` and `SocketBindAllow`, and `app.metrics.yml` (`TestMetricsPort`).
- `benchmarks/RESULTS.md`: measured numbers, each with its date, machine and command.

## Commands

- `go tool task dev`: serve on http://127.0.0.1:8080 with `-dev`, static files as written, and rebuild on every change. The metrics stay on, at http://127.0.0.1:8180/metrics.
- `go tool task test`: vet, gofmt, `go test -race ./...`.
- `go tool task build`: the static binary.
- `go tool templ generate` after editing a `.templ` file. The `_templ.go` files are committed.

## Project rules

Rules that hold for this project only go here.
