package writer

import (
	"database/sql"
	"errors"
	"time"
)

// Once makes cmd idempotent per key, such as an op id from the page
// scoped to its user, so that a retried request gets the first answer.
// When cmd succeeds, Once records the key in the table lith_ops (created
// on demand, in the same savepoint); a command with a recorded key
// succeeds without running cmd again. A failed command changed nothing
// and leaves no record: a retry runs it again and gets its error afresh,
// with its type. PruneOnce forgets old keys.
func Once(key string, cmd Command) Command { return &once{key: key, cmd: cmd} }

type once struct {
	key string
	cmd Command
}

func (o *once) Validate() error {
	if o.key == "" {
		return errors.New("no key for Once")
	}
	if o.cmd == nil {
		return errors.New("no command for Once")
	}
	if v, ok := o.cmd.(Validator); ok {
		return v.Validate()
	}
	return nil
}

func (o *once) Apply(tx *Tx) error {
	if err := tx.w.opsTable(tx); err != nil {
		return err
	}
	var one int
	err := tx.QueryRow("SELECT 1 FROM lith_ops WHERE key = ?", o.key).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := o.cmd.Apply(tx); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO lith_ops (key, at) VALUES (?, ?)", o.key, time.Now().Unix())
	return err
}

// PruneOnce returns a command that forgets the keys Once recorded more
// than age ago. Send it now and then, hourly with an age of a day, say.
func PruneOnce(age time.Duration) Command {
	return Func(func(tx *Tx) error {
		if err := tx.w.opsTable(tx); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM lith_ops WHERE at < ?", time.Now().Add(-age).Unix())
		return err
	})
}

func (w *Writer) opsTable(tx *Tx) error {
	if w.opsReady {
		return nil
	}
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS lith_ops (key TEXT PRIMARY KEY, at INTEGER NOT NULL) STRICT, WITHOUT ROWID"); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS lith_ops_at ON lith_ops (at)"); err != nil {
		return err
	}
	tx.AfterCommit(func() { w.opsReady = true })
	return nil
}
