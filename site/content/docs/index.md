---
title: Documentation
nav: Overview
summary: How lith works, how to start a project on it, and the rules its apps follow.
order: 0
---

lith is a Go module for web apps that keep their state in one SQLite file and draw their pages on the server. Its packages work together and are imported one by one: `sqlite` opens the database, `writer` applies every change, `live` renders the views and streams them, `sse` formats and compresses the streams, and `edge` handles a request before and after its handler.

lith owns that runtime and nothing above it. Routing stays with `net/http`, HTML with [templ](https://templ.guide), and each app keeps its own pages and domain model. In the browser, [Datastar](https://data-star.dev) sends the actions and morphs the HTML that the server streams back into the page.

## Where to start

- [Getting started](/docs/getting-started) makes a project from lith's template and runs it.
- [Concepts](/docs/concepts) explains the four ideas in the name: Linear writes, Immediate mode, Ticks, Hypermedia.
- The [demos](/demos) run on this server, on lith, each with the code that does it.
