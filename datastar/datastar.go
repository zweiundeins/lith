// Package datastar holds the Datastar build that lith's template and site
// serve: Starbase's patched build of Datastar with Rocket, one minified ES
// module. It embeds that file and nothing else, so an app that serves the
// build gains its 72 KB:
//
//	static.Add("vendor/datastar-rocket.js", datastar.Datastar())
//	url := static.URL("vendor/datastar-rocket.js")
//
// VERSION beside the file says where it came from. lith's newest-frame.js
// is live's: live.Scripts and live.NewestFrame.
package datastar

import _ "embed"

// Version names the build that Datastar returns: Datastar v1.0.5 with
// Rocket and the patches in Starbase's patches/rocket, as Starbase's
// commit 818f366 vendors it.
const Version = "1.0.5+starbase.818f366"

//go:embed datastar-rocket.js
var build string

// Datastar returns the build, a copy of its own on every call.
func Datastar() []byte { return []byte(build) }
