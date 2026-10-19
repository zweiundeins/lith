package demo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// TabTTL is how long a tab's state outlives its last write.
const TabTTL = 24 * time.Hour

// TabTopic is the topic of the state of a demo's tab: a command that saves
// it touches the topic, and the tab's own view listens to it. It is under
// tab, not under the demo's slug, where a view on the slug would render
// for every tab's save.
func TabTopic(slug, tab string) string { return "tab:" + slug + ":" + tab }

// SaveTab stores state, as JSON, as the state of the viewer's tab in the
// demo, and touches TabTopic. It runs inside a command.
func SaveTab(tx *writer.Tx, slug string, v Viewer, state any, now time.Time) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO site_tabs (demo, sid, tab_id, data, seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (demo, sid, tab_id) DO UPDATE SET data = excluded.data, seen = excluded.seen`,
		slug, v.Session, v.Tab, string(data), now.Unix()); err != nil {
		return err
	}
	tx.Touch(TabTopic(slug, v.Tab))
	return nil
}

// LoadTab reads the state of the viewer's tab in the demo into state. A tab
// that has no row yet leaves state as it is.
func LoadTab(ctx context.Context, q sqlite.Querier, slug string, v Viewer, state any) error {
	var data string
	err := q.QueryRowContext(ctx, `SELECT data FROM site_tabs WHERE demo = ? AND sid = ? AND tab_id = ?`, slug, v.Session, v.Tab).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), state)
}

// SweepTabs is the site's sweep of tab state: it deletes the rows of tabs
// not written since TabTTL before now.
func SweepTabs(tx *writer.Tx, now time.Time) error {
	_, err := tx.Exec(`DELETE FROM site_tabs WHERE seen < ?`, now.Add(-TabTTL).Unix())
	return err
}

var tableName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func checkTable(names ...string) {
	for _, n := range names {
		if !tableName.MatchString(n) {
			panic(fmt.Sprintf("demo: %q is not a table or column name", n))
		}
	}
}

// KeepNewest deletes the rows of table beyond its newest n by rowid, and
// returns how many it deleted. A demo's Sweep calls it for every table
// that grows with use, and touches its topics when rows went. table is a
// constant of the demo's: each table makes one SQL text, which the
// writer's statement cache keeps.
func KeepNewest(tx *writer.Tx, table string, n int) (int64, error) {
	checkTable(table)
	res, err := tx.Exec(`DELETE FROM `+table+` WHERE rowid <= (SELECT rowid FROM `+table+` ORDER BY rowid DESC LIMIT 1 OFFSET ?)`, n)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOlder deletes the rows of table whose column, in unix seconds, is
// before t, and returns how many it deleted.
func DeleteOlder(tx *writer.Tx, table, column string, t time.Time) (int64, error) {
	checkTable(table, column)
	res, err := tx.Exec(`DELETE FROM `+table+` WHERE `+column+` < ?`, t.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
