// Package datastar holds the Datastar build that lith's template and site
// serve: Starbase's patched build of Datastar with Rocket, one minified ES
// module. It embeds that file and nothing else, so an app that serves the
// build gains its 72 KB:
//
//	static.Add("vendor/datastar-rocket.js", datastar.JS())
//	url := static.URL("vendor/datastar-rocket.js")
//
// A page that pins the build gives Integrity in the script's integrity
// attribute, or in its import map's "integrity" entry when Datastar is
// imported. edge.Static serves it byte for byte, minified or not, so the
// hash holds. VERSION beside the file says where it came from. lith's
// newest-frame.js is live's: live.Scripts and live.NewestFrame.
package datastar

import _ "embed"

// Version names the build that JS returns: Datastar v1.0.5 with
// Rocket and the patch in Starbase's patches/rocket, as Starbase's
// commit 7dc2352 vendors it. A bool prop reads as HTML's boolean
// attributes do, by presence: open="false" is open.
const Version = "1.0.5+starbase.7dc2352"

// Integrity is the build's subresource integrity, its SHA-384 as an
// integrity attribute or an import map's "integrity" entry takes it, for a
// page that pins the build, or an app that checks a copy it vendors.
const Integrity = "sha384-DkjVaBESHDA703Xk1QdsjXyHUmwC4Z9N/T8ak/EVxdTXso6apSccCKzi0DwwbGGK"

//go:embed datastar-rocket.js
var build string

// JS returns the build, a copy of its own on every call.
func JS() []byte { return []byte(build) }
