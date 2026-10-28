---
title: minify
headline: lith/minify: JavaScript and CSS minified at startup
summary: Minifies JavaScript and CSS for edge.Static: JavaScript with esbuild, CSS with tdewolff/minify.
description: Reference for lith/minify: minify JavaScript with esbuild and CSS with tdewolff/minify as edge.Static reads them, before they are hashed and precompressed.
order: 14
---

Import path `github.com/zweiundeins/lith/minify`, a module of its own: `go get github.com/zweiundeins/lith/minify`. Generated from the doc comments in [minify/](https://github.com/zweiundeins/lith/tree/main/minify).

Passed to edge.Minify, Bytes minifies the scripts and stylesheets that Static reads and those that Add gives it, before Static names and precompresses them:

```go
static, err := edge.NewStatic(files, "/static/", edge.Minify(minify.Bytes))
```

It is a module of its own, so that projects that serve their files as written never pull in the minifiers. Leave the option out in development, so that the browser's devtools show the files as written.

## Functions

### func Bytes {#Bytes}

```go
func Bytes(mediatype string, b []byte) ([]byte, error)
```

Bytes returns b minified: JavaScript, scripts and modules alike, for the media type text/javascript, and CSS for text/css; another type is an error. Comments and the spaces a file can do without go, and local names get shorter; comments that start with /\*! or hold @license or @preserve stay. A file with import or export is a module, whose top-level names are local too: what it imports and exports keeps its names. A file without is a classic script, whose top-level names are globals that other scripts may use, so they keep theirs. Strings and template literals keep their text (quotes may change, a string to a template literal and back), and a tagged template's text stays byte for byte, as its tag sees it. JavaScript that does not parse is an error that names its line and column. Nothing esbuild writes of its own is newer than ES2020: newer syntax an author writes stays, except that a ||= b and its kin come out as a || (a = b). CSS keeps its layers, nesting and container queries, and at-rules it does not know, such as @container and @scope, keep their blocks as written; a declaration it cannot parse is copied as it is, as browsers skip it.

It has the signature of edge.Minify's function, and of tdewolff's (\*minify.M).Bytes, which an app with its own minify.M passes instead.
