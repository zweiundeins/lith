---
title: Credits
headline: Credits: what lith learned from and builds on
summary: The projects lith learned from and builds on, and what this site is made of.
description: The projects lith learned from (hyperlith, northstar), what it builds on (Datastar, modernc SQLite, zstd and brotli), and what makes up this site.
order: 8
---

## Where lith comes from

lith is inspired by [andersmurphy/hyperlith](https://github.com/andersmurphy/hyperlith) and [zangster300/northstar](https://github.com/zangster300/northstar).

lith's code started in [kanban](https://github.com/zweiundeins/kanban), [starbase](https://github.com/zweiundeins/starbase) and [Libretto](https://libretto.ch).

## What lith builds on

- [Datastar](https://data-star.dev) runs in the browser of every lith app. The template and this site vendor Starbase's patched build of version 1.0.4.
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) is SQLite in pure Go.
- [klauspost/compress](https://github.com/klauspost/compress) compresses the streams with zstd and gzip, and [andybalholm/brotli](https://github.com/andybalholm/brotli) with brotli.

## What this site is made of

- The site runs on lith itself, in one binary with one SQLite file, from `site/` in lith's repository.
- [templ](https://templ.guide) renders its HTML, [goldmark](https://github.com/yuin/goldmark) its Markdown and [chroma](https://github.com/alecthomas/chroma) the colours of its code.
- [Pixelify Sans](https://github.com/eifetx/Pixelify-Sans) sets the display text and [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) the code, both by their projects' authors under the SIL Open Font License, which ships next to the font files.
- The monolith and the other pixel art are drawn in Go, the way [Starbase](https://starbase.zweiundeins.gmbh) draws its own.
