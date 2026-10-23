package web

import (
	"context"
	"strings"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/views"
)

// refresh is the streams' live.Spec.Refresh, for a page that a former
// build rendered: the parts of the landing page and of a demo's page
// around the demos, rendered afresh by their ids. The demos' islands stay
// as they are, since their views keep them current and they hold the
// demos' signals and live elements. A page that loaded other scripts or
// stylesheets gets a notice that asks for a reload instead: assets is the
// assets version the page reported.
func (s *Server) refresh(ctx context.Context, rq live.RefreshRequest, assets string) ([][]byte, error) {
	path, _, _ := strings.Cut(rq.Page, "?")
	var parts []templ.Component
	var scripts []views.Script
	switch slug, ok := strings.CutPrefix(path, "/demos/"); {
	case path == "/":
		parts = []templ.Component{views.LandingMore(views.Landing{Cards: s.cards(), Repo: s.cfg.Repo})}
		scripts = s.homeComponents()
	case ok && s.demo(slug) != nil:
		d := s.demo(slug)
		parts = []templ.Component{views.DemoHead(d.card), views.DemoServer(d.server), views.DemoHow(d.how)}
		scripts = s.components(d.Components...)
	default:
		return nil, nil // not a page that streams
	}
	if assets != (views.Shell{Assets: s.assets, Components: scripts}).AssetsVersion() {
		ev, err := demo.Patch(ctx, views.Update(true))
		return [][]byte{ev}, err
	}
	events := make([][]byte, len(parts))
	for i, c := range parts {
		ev, err := demo.Patch(ctx, c)
		if err != nil {
			return nil, err
		}
		events[i] = ev
	}
	return events, nil
}
