---
title: datastar
headline: lith/datastar: the Datastar build the template serves
summary: Holds the Datastar build that lith's template and site serve: Starbase's patched build of Datastar with Rocket, one minified ES module.
description: Reference for lith/datastar: Starbase's patched Datastar build with Rocket, which the template serves, in a package that embeds nothing else.
order: 8
---

Import path `github.com/zweiundeins/lith/datastar`. Generated from the doc comments in [datastar/](https://github.com/zweiundeins/lith/tree/main/datastar).

It embeds that file and nothing else, so an app that serves the build gains its 72 KB:

```go
static.Add("vendor/datastar-rocket.js", datastar.Datastar())
url := static.URL("vendor/datastar-rocket.js")
```

VERSION beside the file says where it came from. lith's newest-frame.js is live's: live.Scripts and live.NewestFrame.

## Constants

```go
const Version = "1.0.5+starbase.818f366"
```

Version names the build that Datastar returns: Datastar v1.0.5 with Rocket and the patches in Starbase's patches/rocket, as Starbase's commit 818f366 vendors it.

## Functions

### func Datastar {#Datastar}

```go
func Datastar() []byte
```

Datastar returns the build, a copy of its own on every call.
