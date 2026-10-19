// Package access is a wiki in one database that people share: a space's
// pages, some of them restricted to the people they were granted to. Two
// people on the same route see different lists, so the view that renders
// the list is keyed by the person as well as by the route.
package access

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations is the schema, for sqlite.Options.Migrations.
func Migrations() fs.FS {
	m, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return m
}

// SpaceTopic is touched by every change to a space's pages, whoever may
// read them.
func SpaceTopic(space int64) string { return "space:" + strconv.FormatInt(space, 10) }

// GrantsTopic is touched by every change to what one person may read.
func GrantsTopic(person int64) string { return "grants:" + strconv.FormatInt(person, 10) }

// Page is a page as a list shows it.
type Page struct {
	ID    int64
	Title string
}

// Readable returns the pages of space that person may read, whose title
// contains filter, by title.
func Readable(ctx context.Context, q sqlite.Querier, person, space int64, filter string) ([]Page, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, title FROM pages
		WHERE space_id = ?1
		  AND (restricted = 0 OR id IN (SELECT page_id FROM grants WHERE person = ?2))
		  AND instr(title, ?3) > 0
		ORDER BY title`, space, person, filter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pages []Page
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.ID, &p.Title); err != nil {
			return nil, err
		}
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

// PagesView is the list of a space's pages for one person, filtered as
// the page asked. Its key holds everything Render filters by: the space
// (the route), the person and the filter. The person reaches Render
// through the closure, and the key keeps two people's lists apart.
func PagesView(person, space int64, filter string) live.View {
	return live.View{
		Key:    fmt.Sprintf("pages:%d:person:%d:q=%q", space, person, filter),
		Topics: []string{SpaceTopic(space), GrantsTopic(person)},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			pages, err := Readable(ctx, q, person, space, filter)
			if err != nil {
				return nil, err
			}
			return renderPages(pages), nil
		},
	}
}

// Server streams a space's pages. Person names who sent a request, from
// its session.
type Server struct {
	Hub    *live.Hub
	Person func(r *http.Request) (person int64, ok bool)
}

// Stream is the stream of the route /spaces/{space}. The person comes
// from the session, never from the URL: whoever could put another
// person's id in the URL would watch that person's list.
func (s *Server) Stream(w http.ResponseWriter, r *http.Request) {
	person, ok := s.Person(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	space, err := strconv.ParseInt(r.PathValue("space"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filter := r.URL.Query().Get("q")
	s.Hub.Serve(w, r, live.Spec{Views: []live.View{PagesView(person, space, filter)}})
}

func renderPages(pages []Page) []byte {
	b := []byte(`<ul id="pages">`)
	for _, p := range pages {
		b = fmt.Appendf(b, `<li><a href="/pages/%d">%s</a></li>`, p.ID, html.EscapeString(p.Title))
	}
	return sse.PatchElements(append(b, `</ul>`...))
}

// AddPage adds a page to a space. Everyone who watches the space's list
// may be able to read it, so it touches the space's topic.
type AddPage struct {
	Space      int64
	Title      string
	Restricted bool
}

func (c AddPage) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`INSERT INTO pages (space_id, title, restricted) VALUES (?, ?, ?)`, c.Space, c.Title, c.Restricted); err != nil {
		return err
	}
	tx.Touch(SpaceTopic(c.Space))
	return nil
}

// Grant lets a person read a restricted page. Only that person's lists
// change, so it touches the person's topic, not the space's.
type Grant struct{ Person, Page int64 }

func (c Grant) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO grants (person, page_id) VALUES (?, ?)`, c.Person, c.Page); err != nil {
		return err
	}
	tx.Touch(GrantsTopic(c.Person))
	return nil
}
