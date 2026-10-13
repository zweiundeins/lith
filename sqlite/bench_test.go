package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkView reads a page in one View, as a GET handler does, with the
// read pool's statement cache and without it, from one goroutine and from
// GOMAXPROCS:
//
//	go test ./sqlite -run '^$' -bench View -benchmem
//
// The board is kanban's: ReadBoard's eight queries (joins, lookups and
// the cards) over its seed's big board of 200 cards in 8 lists. The
// guestbook is the lith-new template's page: a count and the newest 20 of
// 1000 entries.
func BenchmarkView(b *testing.B) {
	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "bench.db")
	board := seedBench(b, path)
	pages := []struct {
		name string
		read func(context.Context, *sql.Tx) error
	}{
		{"board", func(ctx context.Context, tx *sql.Tx) error { return readBoard(ctx, tx, board) }},
		{"guestbook", readGuestbook},
	}
	for _, page := range pages {
		for _, v := range []struct {
			name       string
			statements int
		}{{"cached", 0}, {"uncached", -1}} {
			b.Run(page.name+"/"+v.name, func(b *testing.B) {
				d, err := Open(ctx, path, Options{Statements: v.statements})
				if err != nil {
					b.Fatal(err)
				}
				defer d.Close()
				view := func() error {
					return d.View(ctx, func(tx *sql.Tx) error { return page.read(ctx, tx) })
				}
				b.Run("serial", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := view(); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("parallel", func(b *testing.B) {
					b.ReportAllocs()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if err := view(); err != nil {
								b.Error(err)
								return
							}
						}
					})
				})
			})
		}
	}
}

// TestStatementMemory fills a read connection's cache with 15 variants of
// the board's eight queries, each with a comment of its own, and measures
// what a cached statement holds (DBSTATUS_STMT_USED):
//
//	go test ./sqlite -run TestStatementMemory -v
func TestStatementMemory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bench.db")
	board := seedBench(t, path)
	d, err := Open(ctx, path, Options{Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	count := func() (n, bytes int) {
		c, err := d.R.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return len(cached(t, c)), stmtBytes(t, c)
	}
	n0, bytes0 := count()
	for i := range 15 {
		err := d.View(ctx, func(tx *sql.Tx) error { return readBoard(ctx, commented{tx, i}, board) })
		if err != nil {
			t.Fatal(err)
		}
	}
	n, bytes := count()
	per := (bytes - bytes0) / (n - n0)
	t.Logf("%d statements hold %d bytes, %d each: %d KiB for a full cache", n-n0, bytes-bytes0, per, per*128>>10)
	if n-n0 != 120 || per > 8<<10 {
		t.Fatalf("%d statements, %d bytes each", n-n0, per)
	}
}

// commented runs every query with a comment of its own, so that each
// variant is a statement of its own.
type commented struct {
	*sql.Tx
	i int
}

func (c commented) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.Tx.QueryContext(ctx, fmt.Sprintf("%s -- %d", query, c.i), args...)
}

func (c commented) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.Tx.QueryRowContext(ctx, fmt.Sprintf("%s -- %d", query, c.i), args...)
}

// benchSchema is the part of kanban's schema that its board page reads,
// and the guestbook template's entries.
const benchSchema = `
CREATE TABLE users (
	id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE COLLATE NOCASE, name TEXT NOT NULL,
	color TEXT NOT NULL, password_hash TEXT NOT NULL, created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL) STRICT;
CREATE TABLE boards (
	id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name TEXT NOT NULL, position INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX boards_project ON boards(project_id, position);
CREATE TABLE board_members (
	board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (board_id, user_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX board_members_user ON board_members(user_id);
CREATE TABLE lists (
	id INTEGER PRIMARY KEY, board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	name TEXT NOT NULL, position INTEGER NOT NULL, archived_at INTEGER,
	moved_by INTEGER REFERENCES users(id), move_version INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX lists_board ON lists(board_id, position) WHERE archived_at IS NULL;
CREATE TABLE cards (
	id INTEGER PRIMARY KEY, board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	list_id INTEGER NOT NULL REFERENCES lists(id), title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL, due_on TEXT, due_done INTEGER NOT NULL DEFAULT 0, archived_at INTEGER,
	version INTEGER NOT NULL DEFAULT 0, move_version INTEGER NOT NULL DEFAULT 0,
	moved_by INTEGER REFERENCES users(id), created_by INTEGER NOT NULL REFERENCES users(id), created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX cards_list ON cards(list_id, position) WHERE archived_at IS NULL;
CREATE INDEX cards_board ON cards(board_id) WHERE archived_at IS NULL;
CREATE TABLE labels (
	id INTEGER PRIMARY KEY, board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	name TEXT NOT NULL, color TEXT NOT NULL
) STRICT;
CREATE INDEX labels_board ON labels(board_id);
CREATE TABLE card_labels (
	card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
	label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
	PRIMARY KEY (card_id, label_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX card_labels_label ON card_labels(label_id);
CREATE TABLE comments (
	id INTEGER PRIMARY KEY, card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id), body TEXT NOT NULL, created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX comments_card ON comments(card_id, id);

CREATE TABLE entries (id INTEGER PRIMARY KEY, name TEXT NOT NULL, message TEXT NOT NULL, at INTEGER NOT NULL);
`

// seedBench creates the database at path with kanban's seed (four users,
// a project with a small board and a big one) and 1000 guestbook entries,
// and returns the big board's id.
func seedBench(b testing.TB, path string) (board int64) {
	ctx := context.Background()
	d, err := Open(ctx, path, Options{Migrations: files(map[string]string{"001_bench.sql": benchSchema})})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	must := func(err error) {
		if err != nil {
			b.Fatal(err)
		}
	}
	id := func(q string, args ...any) (id int64) {
		must(tx.QueryRowContext(ctx, q+" RETURNING id", args...).Scan(&id))
		return id
	}
	const base = 1788253200 // 2026-09-01 09:00 UTC, kanban's seed time
	var users []int64
	for _, u := range [][3]string{
		{"anna@example.com", "Anna Keller", "#0f766e"}, {"ben@example.com", "Ben Okafor", "#b45309"},
		{"chiara@example.com", "Chiara Rossi", "#7c3aed"}, {"dev@example.com", "Dev Patel", "#be123c"},
	} {
		users = append(users, id(`INSERT INTO users (email, name, color, password_hash, created_at) VALUES (?, ?, ?, 'x', ?)`, u[0], u[1], u[2], base))
	}
	rng := rand.New(rand.NewPCG(42, 7))
	project := id(`INSERT INTO projects (name, created_at) VALUES ('Product launch', ?)`, base)
	verbs := []string{"Fix", "Add", "Polish", "Refactor", "Document", "Test", "Measure", "Remove", "Speed up", "Design"}
	things := []string{"login form", "board header", "card drag preview", "keyboard moves", "label picker", "due date reminder",
		"comment editor", "archive view", "presence avatars", "SQLite backup", "Caddy config", "systemd unit", "error page", "search"}
	for bi, spec := range []struct {
		name  string
		lists []string
		cards int
	}{
		{"Website", []string{"To do", "Doing", "Done"}, 9},
		{"Release 2.0", []string{"Ideas", "Backlog", "Ready", "In progress", "Review", "Testing", "Done", "Released"}, 200},
	} {
		board = id(`INSERT INTO boards (project_id, name, position, created_at) VALUES (?, ?, ?, ?)`, project, spec.name, (bi+1)*1024, base)
		for _, u := range users {
			_, err := tx.ExecContext(ctx, `INSERT INTO board_members (board_id, user_id) VALUES (?, ?)`, board, u)
			must(err)
		}
		var labels []int64
		for _, l := range []string{"Bug", "Feature", "Design", "Docs", "Blocked", "Quick win", "Research", "Ops"} {
			labels = append(labels, id(`INSERT INTO labels (board_id, name, color) VALUES (?, ?, 'grey')`, board, l))
		}
		var lists []int64
		for i, name := range spec.lists {
			lists = append(lists, id(`INSERT INTO lists (board_id, name, position) VALUES (?, ?, ?)`, board, name, (i+1)*1024))
		}
		for c := range spec.cards {
			li := rng.IntN(len(lists))
			desc := ""
			if rng.IntN(3) == 0 {
				desc = "Why: readers asked for it.\n\n- [ ] agree on the scope\n- [ ] build it"
			}
			var due any
			if rng.IntN(4) == 0 {
				due = time.Date(2026, 10, 1+rng.IntN(60), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			}
			card := id(`INSERT INTO cards (board_id, list_id, title, description, position, due_on, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				board, lists[li], verbs[rng.IntN(len(verbs))]+" "+things[rng.IntN(len(things))], desc, (c+1)*1024, due, users[rng.IntN(len(users))], base+c*600)
			for _, l := range rng.Perm(len(labels))[:rng.IntN(3)] {
				_, err := tx.ExecContext(ctx, `INSERT INTO card_labels (card_id, label_id) VALUES (?, ?)`, card, labels[l])
				must(err)
			}
			for k := range rng.IntN(4) {
				_, err := tx.ExecContext(ctx, `INSERT INTO comments (card_id, user_id, body, created_at) VALUES (?, ?, 'Looks good to me.', ?)`,
					card, users[rng.IntN(len(users))], base+c*600+(k+1)*300)
				must(err)
			}
		}
	}
	for i := range 1000 {
		_, err := tx.ExecContext(ctx, `INSERT INTO entries (name, message, at) VALUES (?, ?, ?)`,
			fmt.Sprintf("Guest %d", i), "Lovely place, we will come back next summer.", base+i*60)
		must(err)
	}
	must(tx.Commit())
	return board
}

type (
	benchUser  struct{ ID, Email, Name, Color any }
	benchLabel struct {
		ID          int64
		Name, Color string
	}
	benchCard struct {
		ID, ListID, Version, MoveVersion int64
		Title, DueOn                     string
		DueDone, HasDesc                 bool
		Labels                           []benchLabel
		Comments                         int
	}
	benchList struct {
		ID, MoveVersion int64
		Name            string
		Cards           []benchCard
	}
	benchBoard struct {
		ID, Version, ProjectID int64
		Name, ProjectName      string
		Siblings               [][2]any
		Labels                 []benchLabel
		Members                []benchUser
		Lists                  []benchList
	}
)

// readBoard is kanban's ReadBoard: the queries of its board page.
func readBoard(ctx context.Context, q Querier, id int64) error {
	var b benchBoard
	err := q.QueryRowContext(ctx, `SELECT b.id, b.name, b.version, p.id, p.name FROM boards b JOIN projects p ON p.id = b.project_id WHERE b.id = ?`, id).
		Scan(&b.ID, &b.Name, &b.Version, &b.ProjectID, &b.ProjectName)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM boards WHERE project_id = ? ORDER BY position, id`, b.ProjectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r [2]any
		if err := rows.Scan(&r[0], &r[1]); err != nil {
			rows.Close()
			return err
		}
		b.Siblings = append(b.Siblings, r)
	}
	rows.Close()
	if rows, err = q.QueryContext(ctx, `SELECT id, name, color FROM labels WHERE board_id = ? ORDER BY name, id`, id); err != nil {
		return err
	}
	labelByID := map[int64]benchLabel{}
	for rows.Next() {
		var l benchLabel
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			rows.Close()
			return err
		}
		b.Labels = append(b.Labels, l)
		labelByID[l.ID] = l
	}
	rows.Close()
	if rows, err = q.QueryContext(ctx, `
		SELECT u.id, u.email, u.name, u.color FROM users u
		JOIN board_members m ON m.user_id = u.id AND m.board_id = ?
		ORDER BY u.name`, id); err != nil {
		return err
	}
	for rows.Next() {
		var u benchUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Color); err != nil {
			rows.Close()
			return err
		}
		b.Members = append(b.Members, u)
	}
	rows.Close()
	if rows, err = q.QueryContext(ctx, `SELECT id, name, move_version FROM lists WHERE board_id = ? AND archived_at IS NULL ORDER BY position, id`, id); err != nil {
		return err
	}
	listIdx := map[int64]int{}
	for rows.Next() {
		var l benchList
		if err := rows.Scan(&l.ID, &l.Name, &l.MoveVersion); err != nil {
			rows.Close()
			return err
		}
		listIdx[l.ID] = len(b.Lists)
		b.Lists = append(b.Lists, l)
	}
	rows.Close()
	cardLabels := map[int64][]benchLabel{}
	if rows, err = q.QueryContext(ctx, `
		SELECT cl.card_id, cl.label_id FROM card_labels cl
		JOIN cards c ON c.id = cl.card_id
		WHERE c.board_id = ? AND c.archived_at IS NULL
		ORDER BY cl.card_id, cl.label_id`, id); err != nil {
		return err
	}
	for rows.Next() {
		var c, l int64
		if err := rows.Scan(&c, &l); err != nil {
			rows.Close()
			return err
		}
		cardLabels[c] = append(cardLabels[c], labelByID[l])
	}
	rows.Close()
	comments := map[int64]int{}
	if rows, err = q.QueryContext(ctx, `
		SELECT cm.card_id, count(*) FROM comments cm
		JOIN cards c ON c.id = cm.card_id
		WHERE c.board_id = ? AND c.archived_at IS NULL
		GROUP BY cm.card_id`, id); err != nil {
		return err
	}
	for rows.Next() {
		var c int64
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			rows.Close()
			return err
		}
		comments[c] = n
	}
	rows.Close()
	if rows, err = q.QueryContext(ctx, `
		SELECT id, list_id, title, version, move_version, coalesce(due_on, ''), due_done, description != ''
		FROM cards WHERE board_id = ? AND archived_at IS NULL
		ORDER BY list_id, position, id`, id); err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var c benchCard
		if err := rows.Scan(&c.ID, &c.ListID, &c.Title, &c.Version, &c.MoveVersion, &c.DueOn, &c.DueDone, &c.HasDesc); err != nil {
			return err
		}
		c.Labels, c.Comments = cardLabels[c.ID], comments[c.ID]
		if i, ok := listIdx[c.ListID]; ok {
			b.Lists[i].Cards = append(b.Lists[i].Cards, c)
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 200 || len(b.Lists) != 8 || len(b.Members) != 4 || len(b.Labels) != 8 {
		return fmt.Errorf("read %d cards in %d lists, %d members, %d labels", n, len(b.Lists), len(b.Members), len(b.Labels))
	}
	return nil
}

type benchEntry struct {
	ID            int64
	Name, Message string
	At            time.Time
}

// readGuestbook is the template's page read (guestbook.Latest): how many
// entries there are, and the newest 20.
func readGuestbook(ctx context.Context, tx *sql.Tx) error {
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&total); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name, message, at FROM entries ORDER BY id DESC LIMIT ?`, 20)
	if err != nil {
		return err
	}
	defer rows.Close()
	var list []benchEntry
	for rows.Next() {
		var e benchEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.Name, &e.Message, &at); err != nil {
			return err
		}
		e.At = time.Unix(at, 0)
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total != 1000 || len(list) != 20 {
		return fmt.Errorf("%d entries, %d read", total, len(list))
	}
	return nil
}
