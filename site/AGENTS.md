# lith-site: notes for coding agents

lith's website, https://lith.zweiundeins.gmbh: the landing page, the docs and the live demos. It is a lith app itself, a module of its own in `site/` that runs on the lith of its own commit (`replace github.com/zweiundeins/lith => ../`). Read lith's skill first, `../skills/lith/SKILL.md` and its references: its rules shape every change here (one writer, commands that touch topics, views rendered once per tick, outcomes on the stream, tab state in SQLite, GET never writes).

## Running and testing

All commands run in `site/`.

- `go tool task dev`: serves http://127.0.0.1:8097 (metrics on 127.0.0.1:8197) and rebuilds on every change (air). It runs with `-dev`: the docs are read from `content/docs` on every request and drafts show.
- `go tool task test`: vet, gofmt, `go test -race ./...`. The gofmt on PATH may be Go 1.26's, which rejects lith's generic methods: the task uses `$(go env GOROOT)/bin/gofmt`.
- Tests that drive headless Chromium (`internal/app`: `TestBrowser`, `TestStarbaseComponents`, `TestTransitionTypes`) skip without Chromium; on a machine without a sandbox, set `LITH_CHROME_NO_SANDBOX=1`.
- `go tool task shots`: screenshots of the pages and of their transitions in `tmp/shots`. Look at your pages before you call them done.
- `go tool templ generate` after editing a `.templ` file; the `_templ.go` files are committed.
- `go tool task build`: the static binary. `deploy/README.md` installs and deploys it.
- `go tool task docs` (`go generate ./content`): writes the docs' generated parts, the package reference and the charts drawn from lith's benchmark CSVs ([The docs](#the-docs)).

## Packages

- `cmd/lith-site`: the binary. Flags, each also `LITH_SITE_<FLAG>`; the public listener and the metrics listener (loopback, the public port + 100); the shutdown order.
- `internal/app`: the wiring, as in lith's template: SQLite through a timed driver (dbmetrics), the writer, the live hub, the edge, `/healthz` and `/readyz`, the metrics registry, the sweep. `internal/app/apptest` runs the whole site in process for tests.
- `internal/web`: routes, pages, the tabs' streams, static files, `robots.txt` and the sitemap. `Stylesheets` in `assets.go` is the CSS bundle's order: a new stylesheet goes there.
- `internal/views`: templ. The frame (`Document`, `Header`, `Menu`, `Footer`), the landing page, the docs, the demo pages, and helpers for demos: `Art`, `Icon`, `Count`.
- `content/docs` and `internal/docs`: the docs, one directory per section, read by the loader (front matter: `title`, `summary`, `order`, `nav`, `draft`); `internal/markdown` renders them. `internal/docs/example` is the app the guides list code from, `internal/docs/gen` and `cmd/docgen` write the reference and the charts' rows ([The docs](#the-docs)).
- `internal/demo`: the kit every demo is built with. `internal/demos`: the list. `internal/demos/<slug>`: one demo each. `internal/demo/demotest`: a database and a writer for a demo's tests.
- `internal/migrate` and `migrations`: migrations in scopes (below).
- `internal/starbase`, `cmd/vendor-starbase` and `internal/web/static/vendor/starbase`: Starbase's components (below).
- `internal/pixelart`: the pixel art (the monolith, the demos' glyphs, the PNGs), `internal/metrics`: the HTTP metrics, `internal/browser`: headless Chromium for tests.

## Adding a demo

A demo is one package under `internal/demos/<slug>` that exports a `demo.Demo`, and one line in `demos.All` (`internal/demos/demos.go`). The guestbook (`internal/demos/guestbook`) is the example to copy. `demo.Demo` holds:

- `Slug`: lowercase letters, digits and `-`. It names the demo everywhere: its page `/demos/<slug>`, its stream `/demos/<slug>/stream`, its actions `/cmd/<slug>/<name>`, its migration scope, the prefix of its tables (`<slug>_`, with `-` as `_`), of its topics and view keys (`<slug>:`), of its view-transition names (`<slug>-`) and its signals (`$_<slug in camelCase>`, `Demo.SignalName`).
- `Title`, `Summary` (one line), `Art` (a pixel glyph: `pixelart.SVG` of a `pixelart.Sprite`).
- `Migrations`: `NNN_name.sql` files in the demo's own `migrations/` folder, numbered from 001 within the demo. Every scope numbers on its own (`site_migrations` records them), so demos built in parallel never clash. Never edit a file that has shipped; add one.
- `Server` and `How`: Markdown, embedded from `server.md` (what happens on the server, beside the demo) and `how.md` (the "How it works" panel). A listing in them with `title=<file>` must be that file's code, whitespace aside: `TestNotes` fails otherwise.
- `CSS`: embedded from `<slug>.css`. Its rules sit inside `@scope ([data-demo="<slug>"])`, the island the site puts the demo in; the site adds it to its bundle in the components layer. Rules for view-transition pseudo-elements go outside the scope (they belong to the document).
- `Mount`: the routes, through the kit: `k.Command(name, demo.Rate{Every, Burst}, handler)` mounts `POST /cmd/<slug>/<name>` behind the CSRF check and the rate limits (the site's 20 a second per address, then the command's rate per session and four times it per address). `k.Get(path, h)` mounts `GET /demos/<slug>/<path>`.
- `Page`: `Render` draws the demo into the GET document from its read transaction; `Views` are the `live.View`s the tab's stream watches. Both read the same state.
- `Embed`: a small live part for a slot of the landing page (below).
- `Sweep`: keeps the demo's tables small, run as a command every five minutes. `demo.KeepNewest(tx, table, n)` and `demo.DeleteOlder(tx, table, column, t)` do the usual cases; touch your topics when rows went.
- `Components`: the Starbase components the demo renders, by slug (below).

### Commands and handlers

A handler decodes the payload (`demo.Decode`), finds who sent it (`demo.Who(r, tab)`: the session and the tab id from the payload), builds the command and calls `k.Do(w, r, cmd)`, which answers 204, 400 with the message of a failed `Validate`, 503 while the server stops, or 500. Wrap a command that a retry must not repeat in `writer.Once(slug+":"+session+":"+op, cmd)`. The page learns the outcome from its stream, never from the answer.

A command reads and writes in its `Apply(tx *writer.Tx)`, touches the topics it changed (`<slug>`, `<slug>:board`, and `demo.TabTopic(slug, tab)` for a tab's own state), and stores what only one tab should hear with `demo.SaveTab`. A refusal is not an error: store it in the tab's state, so the tab's view shows it next to where the user acted.

### Views and the page

- A shared view's key carries nothing of the viewer (`guestbook:entries`); a tab's own view is keyed by `demo.TabTopic(slug, tab)` and renders from `demo.LoadTab`.
- `demo.Patch(ctx, component)` renders an event that morphs elements by their ids, `demo.Signals(v)` one that patches signals, `demo.Querier(ctx, tx)` runs queries on the render worker's prepared statements.
- Who is here: `demo.Watching(ctx, key)` counts the streams on a view and `demo.Watchers(ctx, key)` lists their tab ids, with the ctx of a `Part`'s Render, a view's Render or a sweep (`tx.Context()`); a view that shows them sets `Presence: true`. Never keep the hub in a package variable: tests run several sites in one process.
- The page's signals live under one local object, `$_<slug>` (`data-signals="{_guestbook: {...}}"`), and actions send `$_tab`, the page's tab id. A button that acts makes an op id, stays pending (`aria-busy`, read-only inputs) until the tab's view patches the same op id back, and shows a failed request (`datastar-fetch` with `error`) next to the form: the guestbook's `expr.go` has each expression.
- Inputs in a region the stream morphs need `data-ignore-morph` and their value in a signal; keep forms out of morphed regions, as the guestbook does.

### The landing page's slots

`Embed{Slot, Part}` puts a small live version of a demo beside one of the four ideas (`demo.SlotLinear`, `SlotImmediate`, `SlotTicks`, `SlotHypermedia`), or behind the hero (`demo.SlotHero`, the cursor party). The landing page's stream (`GET /stream`) watches every embed's views. One demo per slot (`demos.Check`). The guestbook holds `SlotImmediate` for now; a demo that fits a slot better takes it by changing that line. A free slot shows a listing.

The hero's slot is `.hero__party`: a full-width layer behind the hero's content, `position: absolute; inset: 0`, a size container named `party` (place things with `cqi` and `cqb`). The layer lets pointers through and its children take them; the hero's links and buttons stay above it.

### Tests for a demo

- Commands and views: `demotest.Open(t, Demo)` gives a migrated database and a running writer; `Touched()` returns the topics committed (see `guestbook_test.go`).
- Streams over HTTP: `apptest.Start(t)` runs the site; `internal/app/app_test.go` has `listen`, `next` and `post`.
- In Chromium: `internal/browser` (pages in separate browser contexts, so separate sessions; `Eval`, `Wait`, `Press`, `Screenshot`, `Errors`). A demo is done when two pages see each other's changes and the console stays empty.

## Design system

- **Tokens.** Primitives in `css/tokens.css` (`--lith-stone-0` to `-13`, `--lith-ember-1` to `-10`, space, type, motion); semantic tokens in `css/theme.css`, each with its light and its dark value in `light-dark()`. Components use semantic tokens only: `--lith-bg`, `--lith-surface-1`, `-2`, `-inset`, `-hover`, `--lith-border`, `-subtle`, `-strong`, `--lith-text-1`, `-2`, `-muted`, `--lith-accent` (fills, with `--lith-on-accent` on them), `--lith-accent-text` (the accent as text), `--lith-accent-subtle`, `--lith-glow-color`, `--lith-danger`, `--lith-ok`, `--lith-focus`, `--lith-code-*`. `TestContrast` checks every text and surface pair in both themes; add the pairs of a new token.
- **Themes.** `data-lith-theme` (dark by default, `light`, `system`) and `data-lith-style` (`pixel`, `smooth`) on `<html>`, from the cookies `lith-theme` and `lith-style`, so the server renders them. Never branch on the theme: the tokens carry it.
- **8-bit details.** `--lith-notch` (1, or 0 in the smooth style) notches corners: the `.notched` class, or its clip-path polygon (a clip-path cuts outer outlines, so focus rings on notched elements sit inside). `--lith-frame-step` sizes `.pixel-frame`, `--lith-font-display` is Pixelify Sans, or the reading face when smooth.
- **Fonts.** Pixelify Sans for display text, JetBrains Mono for code and small UI labels (`--lith-font-ui`), the system face for reading.
- **Classes to use.** `.btn` (`--primary`, `--ghost`, `--sm`; `aria-busy="true"` shows the pending pixel), `.icon-btn`, `.field` with `.input`, `.outcome` (`.is-refused`), `.panel`, `.notched`, `.pixel-frame`, `.tip` with `.tip__bubble` (a tooltip by anchor positioning), `.code` (`markdown.CodeBlock`), `.prose`, `.live-badge`, `.card-grid`, `.eyebrow`, `.muted`, `.visually-hidden`.
- **Layers.** `@layer reset, tokens, theme, base, layout, components, utilities`. Every rule sits in a layer; an unlayered rule beats all of them. Demo CSS lands in `components`.
- **Layout.** Container queries, not media queries: `main` is the container `page`; demo stages are `stage`. A grid that can hold code gets `grid-template-columns: minmax(0, 1fr)`, or its track grows to the longest line.

Where the site uses the newer CSS: `@scope` for demo islands, `@property` for the seam's glow (`--lith-seam-glow`) and the button's sweep, `@starting-style` for arriving entries and the display menu, anchor positioning for the menu and tooltips, the popover API for the display menu, scroll-driven reveals (`.reveal`) and the docs' reading bar, a style query on the numbers band (`@container style(--band: raised)`), `:has()` for fields and cards, `interpolate-size` for "How it works", cross-document view transitions everywhere.

## Motion

- Durations `--lith-dur-1` to `-5` (120 to 640 ms) and curves with one job each: `--lith-ease-glide` moves things into place, `--lith-ease-zoom` opens pages, `--lith-ease-reveal` wipes, `--lith-ease-snap` answers a press, `--lith-ease-exit` leaves, `--lith-ease-steps` for pixel motion.
- `prefers-reduced-motion: reduce` turns everything off: the reset layer cuts every animation and transition, and the view transitions stop. Test with it on.
- Motion never runs under text someone reads, and nothing loops on content: indicators only (the live dot, the pending pixel, the seam).
- An element that arrives from the stream materialises with `@starting-style` (the guestbook's `.gb__entry[data-fresh]`). Mark a section `.reveal` and it rises in as it scrolls into view.

## View transitions

Every navigation is a cross-document view transition. `static/js/transitions.js` gives each one a type in `pagereveal`: `forward` and `backward` between docs pages by their order in the navigation, `zoom-in` from the landing page or `/demos` into a page and `zoom-out` back, `fade` for the rest. The theme and corner switch is a same-document transition of type `theme`, a circle from the switch. A transition without a type is a stream's patch: the page swaps at once and only named elements move.

Names, unique on a page:

- The site's: `site-header`, `logo`, `docs-nav`, `docs-nav-marker`, `page-title`.
- A demo's card and the head of its page: `demo-<slug>`, `demo-<slug>-title`, `demo-<slug>-art` (transitions.js names the card the moment a reader follows it).
- Inside a demo: `<slug>-<thing>-<id>`, with `view-transition-class: <slug>-<thing>`, and the rules for `::view-transition-group(.<slug>-<thing>)` in the demo's CSS, outside its `@scope`.

A view whose patch should animate returns `demo.Animated(ctx, component, sse.Patch{ViewTransitionSelector: "#<id>"})`: the stream sends its first frame without a transition (the page already shows it, and a transition then would cut short the navigation's) and every later one inside one (Datastar's `useViewTransition`), scoped to the element the selector names, usually the one the frame patches (`#guestbook-entries`). The scope is required: while a transition of the whole document runs, Chromium gives every press on the page to `<html>`, so every button and link would miss its clicks. Inside the scope nothing takes a press while it runs: Chromium gives it to what lies beneath the scope (the board finds the card under a press by its box). The pseudo-elements belong to the scope element, so `::view-transition-group(.<slug>-<thing>)` matches as written, and a rule that names the originating element names the scope (`#board-lanes::view-transition-group(*)`), never `html`. Give the elements that move a name: the guestbook's entries glide down when one arrives; a shared board's cards with `view-transition-name: board-card-<id>` glide between lanes when someone else moves one. Use it for views that change a few times a second at most. High-frequency frames (cursors, a Game of Life) animate with CSS only: a transition freezes its scope while it runs, and a new one cuts the last one short.

Chromium-based browsers get all of it, which `TestTransitionTypes` checks. Safari runs cross-document transitions from 18.2; where the Navigation API's activation is missing, transitions.js takes the page it came from from the referrer. A browser with same-document transitions only (Firefox, for now) gets the theme switch, and navigates without a transition. The streams' patches animate only where elements have `startViewTransition` (scoped transitions, in Chromium 154 and probably earlier); elsewhere they morph at once. Without any, everything changes at once.

## Live views and reconnecting

- A page that streams sets `Shell.Stream`: the frame renders `<div id="stream" data-indicator:_live data-init="@get(...)">` with `retry: 'error'`. Its id is the stream's `live.Spec.Reconnect`: when the server stops, each stream's last event re-creates that element and the page reconnects to the next process. `$_live` is true while the stream is open (the "live" badge reads it).
- One stream per tab (`Spec.ID` is the tab id); the site writes each tick's frames in one write (`frameWriter`).
- The hub hibernates a stream's encoder after a minute without frames and shares a memory budget among the encoders (the default: half of GOMEMLIMIT). Keep frames well under 256 KB, the smallest window the budget gives.
- What must survive a deploy is a row, written by a command: a tab's state in `site_tabs` (`demo.SaveTab`), never a map in memory.

## Memory

The unit runs with `MemoryMax=256M` and `GOMEMLIMIT=192MiB` beside other apps on a host with about 1.3 GB free. Measured with 200 open streams: 110 MB resident, 100 MB of it the encoders' budget (115 streams at zstd 2^18, the rest uncompressed). So:

- Nothing per tab in memory; no unbounded caches. Every table that grows has a sweep.
- Static files are compressed at brotli -11 when the process starts: a 1.1 MB file costs about 85 MB of heap for a moment, and Go keeps it. That is why ECharts is not vendored; think twice before adding any large file.
- `-streams 300` caps the open streams. A demo with many tabs and large frames should measure (`lith_site_sse_budget_*`).

## Starbase's components

The site vendors Rocket components from Starbase as one-file bundles, pinned to a catalog snapshot, each checked against the snapshot's integrity: `internal/web/static/vendor/starbase` (`pins.json` the wish list, `lock.json` what was vendored, `TestVendored` checks the files against it). The bundles import `'datastar'`, which the page's import map points at the site's Datastar build, the patched build Starbase pins (`cmd/vendor-starbase` refuses a snapshot with another). A page loads the bundles of the components it names, and of the components they render; `Demo.Components` names a demo's. `css/starbase.css` maps Starbase's `--sb-*` tokens onto the site's, so the components follow both themes and the smooth style.

Vendored: `sb-drawer` (the site's menu on narrow screens), `sb-tabs` (the quick start), `sb-toast`, `sb-tooltip`, `sb-popover`, `sb-virtual-scroll`, `sb-pixel-board`, `sb-sparkline`, `sb-meter`, `sb-gauge`, `sb-data-table`. Their READMEs are in Starbase's repository, `components/<slug>/README.md`, and on https://starbase.zweiundeins.gmbh/components/<slug>.

- `sb-virtual-scroll`: a list or grid of any length that holds only the rows in view; the million-checkbox grid. The server sends the host with `offset`, `total` and the items; `sb-window` asks for a window. In a lith page the window is the tab's state: `sb-window` posts a command that stores it, and the tab's view renders the host with its items. `columns` makes a grid. Items carry no `id`.
- `sb-pixel-board`: the shared pixel canvas. The server renders `cells` (one hex digit per cell) into every frame; painting emits `sb-paint` with `{color, cells}`, posted with `requestCancellation: 'disabled'` so a stroke drops nothing, and rate-limited per session and address. No `data-preserve-attr` on `cells`: every morph brings the latest board.
- `sb-sparkline`, `sb-meter`, `sb-gauge`: live numbers (the writer race, the stream inspector). A server-rendered `value` attribute, or `data-attr:value` from a signal the stream patches together with `data-preserve-attr="value"`. The sparkline appends a point whenever its value changes.
- `sb-data-table`: rows the server keeps, a window at a time, sorted on the server (`sb-window`, `sb-sort`), with selection. It renders `sb-virtual-scroll`.
- `sb-toast`: feedback the server owns: `toasts` is the list, the viewer dismisses locally.
- `sb-tooltip`, `sb-popover`: a hint or a panel by a trigger. The site's own tooltips are CSS (`.tip`), which is often enough.
- `sb-echarts` is not vendored: its library is 1.1 MB (see Memory). Prefer the pixel charts; a demo that needs ECharts measures the start-up cost first.

To add a component: put its slug in `pins.json` (or move `catalog` to a newer snapshot, from a component page's install snippet), run `go tool task vendor-starbase`, commit the folder, and name it in `Demo.Components`. Starbase's rules that matter:

- A value attribute the server sends again wins over the user's edit only when it changed (server wins); to clear, send an empty value, never drop the attribute.
- `data-bind` on a component writes only to a signal that exists: declare it with `data-signals` first, and bind with `__prop` (`data-bind:_x__prop.value`).
- Elements with ids that hold Rocket components and move or repeat crash Datastar 1.0.4's morph; the patched build the site vendors fixes it, so keyed elements are safe here.
- Attributes a signal drives (`data-attr:x`) need `data-preserve-attr="x"`, or the next morph resets them.
- A component's stream opens with `retry: 'error'` from an element with an id, like the page's, if it has one of its own.

## Monitoring

`/metrics` listens on 127.0.0.1 at the public port + 100, never on the public listener. It serves `lith_site_http_requests_total{route,code}` and `lith_site_http_request_duration_seconds{route}` (the route is the ServeMux pattern, so keep patterns fixed: never a raw path in a label), lith's `lith_site_writer_*`, `lith_site_live_*` and `lith_site_sse_budget_*`, the statements' `lith_site_db_*`, `go_*` and `process_*`. `deploy/observability` has the Grafana dashboard for lith apps and their alert rules; `TestDashboard` fails when they read a metric the site does not export. Logs are slog JSON on stdout.

## The docs

`content/docs/<section>/index.md` leads a section, its other `.md` files follow by `order`; `{#id}` after a heading sets its id (the reference's `Writer.Do`). The sections: Getting started, Concepts, Guides, Datastar notes, Performance, Package reference, Review checklist, Credits. Every page is in the navigation, whose order `transitions.js` reads for the direction of a page's transition.

- **The guides' code is real.** The guides follow one app, boards of lanes and cards, in `internal/docs/example`: `board` (commands, queries, schema), `web` (handlers, views, the page, a load test), `tenants` (a pool of databases), and `backup` (the Litestream wiring, a module of its own so that the site never pulls in Litestream; `TestBackupExample` runs `go vet` in it). The site's `go vet` and tests compile and run the rest. A fenced block with `source=<path from lith's root>` in its info string is a listing: the caption links the file, and `TestListings` fails unless the block, whitespace aside, is in that file. List the example, lith's template, or this site's own code; change the code first, then copy it into the page.
- **Generated parts.** go:embed cannot reach outside the site's module, so `cmd/docgen` (`go tool task docs`) copies in what comes from the rest of the repository: `content/docs/reference/*.md`, one page per package of `gen.Packages`, from the Go doc comments through go/doc (signatures with their field comments, examples from `_test.go` files), and the rows of every chart whose spec names `columns:`. Never edit those by hand. `TestUpToDate` (`internal/docs/gen`) fails when a doc comment, a package or a CSV changed and the docs did not: run the task and commit the result. A new package goes into `gen.Packages`.
- **Charts.** A ```` ```chart ```` block is a spec (`kind: bars|lines`, `title`, `x`, `y`, `source`, optional `x-prefix`, `x-suffix`) and, after a `---` line, CSV rows of series, x and y. `internal/markdown` draws it as inline SVG with a legend and a table of the numbers, coloured by the four series hues in `docs.css` (checked for colour vision deficiencies against the inset surface of both themes; a fifth series is an error). Rows are typed in, from `source`, or filled by docgen from a CSV file with `columns: series, x, y`, `where: col=value, col!=value`, `series: label=value; ...` and `step:`. `TestChartNumbers` fails when a number a chart draws is not in its source file.
- **One source of truth with the skill.** `skills/lith` teaches coding agents what these pages teach people. The two registers differ (the skill is terse rules with the projects' names, the pages explain the symptom, the cause and the fix), so neither is generated from the other: both are written, and a test ties them. `TestSkill` (`internal/docs/skill_test.go`) holds them together from the skill's side: every `rg` command of `references/review.md` must be on `/docs/review`, every code span of `references/datastar.md` on `/docs/datastar`, and every section of `references/recipes.md` and `references/performance.md` must have a page that covers it (the map in the test). A change to a fact on either side changes the other in the same commit; the test catches what the skill gains and the docs lack, not the reverse.
- **Numbers.** A number on a performance page comes from `bench/RESULTS.md` or `bench/budget`, with its machine and date; a number measured for a guide says when and where it was measured.
- **Screenshots.** `TestShotsDocs` (`internal/app/docs_shots_test.go`, part of `go tool task shots`) shoots the sections on desktop and mobile, in both themes, and each chart of the writer's and the budget's pages close up.

## Writing

The site's copy and docs are plain, concrete English: numbers, names, mechanisms. No em or en dashes, headings that name their subject, no hype words. A docs page says what is true of the code in this commit; `TestContent` fails on a broken link inside the site or a long dash.
