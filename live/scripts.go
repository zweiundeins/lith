package live

import "embed"

// Scripts holds the scripts lith gives the page: newest-frame.js
// (NewestFrame). Serve them with the app's other static files, by a name
// edge.Static makes from their content and as immutable:
//
//	js, err := fs.ReadFile(live.Scripts, live.NewestFrame)
//	if err != nil {
//		return err
//	}
//	static.Add("lith/"+live.NewestFrame, js)
//	url := static.URL("lith/" + live.NewestFrame) // /static/lith/newest-frame.3f2a9c1b2d4e.js
//
//go:embed newest-frame.js
var Scripts embed.FS

// NewestFrame names newest-frame.js in Scripts, a Datastar plugin that does
// in the page what the hub does for a stream: the newest frame of a view
// wins. lith vendors its build from
// github.com/mbolli/datastar-attribute-newest-frame, which has its source,
// tests and documentation; VERSION next to it names the commit. A page
// loads it as a module after Datastar, through an import map that maps
// 'datastar' to Datastar's URL, and turns it on with
// data-newest-frame__reopen on its <body>.
//
// It holds the patch events of the stream that the element #stream opened
// (data-newest-frame:feed__reopen names another id), and passes them on to
// Datastar once each chunk of the stream is parsed, or at the next
// animation frame when it passed some on less than a frame before, and no
// sooner after a batch than half the time that batch took to apply. Of the
// element patches in such a batch that replace the same elements wholly,
// only the newest is applied: mode outer or replace of elements that all
// have ids, or mode inner with an id selector, without a script or a view
// transition. Signal patches and every other patch keep their order and
// are never dropped. A page that keeps up applies every frame.
//
// A page whose main thread is too slow for its stream falls behind, and the
// hub cannot tell: the browser and the kernel hold minutes of frames, and
// the server's writes never wait. Chromium then hands the page 2 MiB of
// the stream at each read, and Datastar applies all of it in one task
// without a paint between. Firefox holds the frames in its parent process
// and hands the page one at a time while its main thread is free, which
// the pause after a long batch leaves time for. With the plugin the page
// applies each view's newest frame instead and stays within about a
// second. When more than half of the frames of the last 3 s were dropped,
// at least 10, it dispatches newest-frame-behind on the stream's element,
// and with __reopen it sets $_slow and opens the stream again from a copy
// of its element. The page's stream sends Save-Data: on while $_slow is
// true, so the handler's edge.SlowLink gives it a slow stream (Spec.Slow)
// at the same URL, and Datastar aborts the old request. It asks once; a
// page that measured a slow link as it started never asks.
const NewestFrame = "newest-frame.js"
