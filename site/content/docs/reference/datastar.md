---
title: datastar
headline: lith/datastar: the Datastar build the template serves
summary: Holds the Datastar build that lith's template and site serve: Starbase's patched build of Datastar with Rocket, one minified ES module.
description: Reference for lith/datastar: Starbase's patched Datastar build with Rocket, which the template serves, with its version and integrity hash.
order: 8
---

Import path `github.com/zweiundeins/lith/datastar`. Generated from the doc comments in [datastar/](https://github.com/zweiundeins/lith/tree/main/datastar).

It embeds that file and nothing else, so an app that serves the build gains its 72 KB:

```go
static.Add("vendor/datastar-rocket.js", datastar.JS())
url := static.URL("vendor/datastar-rocket.js")
```

A page that pins the build gives Integrity in the script's integrity attribute, or in its import map's "integrity" entry when Datastar is imported. edge.Static serves it byte for byte, minified or not, so the hash holds. VERSION beside the file says where it came from. lith's newest-frame.js is live's: live.Scripts and live.NewestFrame.

## Constants

```go
const Integrity = "sha384-Iu4CMxrmjOgH23gCxLjuY70tjpaQn5TFNBOARkUXz3xmm7MxDUz+XptuiVQFLxvO"
```

Integrity is the build's subresource integrity, its SHA-384 as an integrity attribute or an import map's "integrity" entry takes it, for a page that pins the build, or an app that checks a copy it vendors.

```go
const Version = "1.0.5+starbase.818f366"
```

Version names the build that JS returns: Datastar v1.0.5 with Rocket and the patches in Starbase's patches/rocket, as Starbase's commit 818f366 vendors it.

## Functions

### func JS {#JS}

```go
func JS() []byte
```

JS returns the build, a copy of its own on every call.
