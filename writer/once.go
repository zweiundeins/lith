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

// PruneOnce returns a command that forgets the keys Once and OnceResult
// recorded more than age ago. Send it now and then, hourly with an age of
// a day, say.
func PruneOnce(age time.Duration) Command {
	return Func(func(tx *Tx) error {
		if err := tx.w.opsTable(tx); err != nil {
			return err
		}
		at := time.Now().Add(-age).Unix()
		if _, err := tx.Exec("DELETE FROM lith_ops WHERE at < ?", at); err != nil {
			return err
		}
		var n int
		err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'lith_results'").Scan(&n)
		if err != nil || n == 0 {
			return err
		}
		_, err = tx.Exec("DELETE FROM lith_results WHERE at < ?", at)
		return err
	})
}

// ResultCommand is a command with an answer besides its error: a small
// result, such as the id of a row it created or an outcome as JSON.
type ResultCommand interface {
	ApplyResult(*Tx) ([]byte, error)
}

// ResultFunc is a ResultCommand made of a function.
type ResultFunc func(*Tx) ([]byte, error)

// ApplyResult calls f.
func (f ResultFunc) ApplyResult(tx *Tx) ([]byte, error) { return f(tx) }

// Refusal is a command's refusal, with the message the user reads.
// OnceResult records it like a result.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// OnceResult is Once for a command with an answer: it records the key
// with the command's result, or with its refusal, an error that is a
// *Refusal (errors.As). A refusal undoes the command's changes, as any
// error does, but its record stays. A command with a recorded key does
// not run again: Do returns the recorded refusal, as a *Refusal with its
// message, and Result the recorded result. Other errors leave no record,
// so a retry runs the command again. PruneOnce forgets old keys.
func OnceResult(key string, cmd ResultCommand) *Op { return &Op{key: key, cmd: cmd} }

// Op is a command made by OnceResult. Read its answer once Do returned.
type Op struct {
	key      string
	cmd      ResultCommand
	result   []byte
	replayed bool
}

// Result is the command's result, from this run or a recorded one.
func (o *Op) Result() []byte { return o.result }

// Replayed reports whether the answer is a recorded one, from an earlier
// run with the key.
func (o *Op) Replayed() bool { return o.replayed }

// Validate checks the key and the command, and runs the command's own
// Validate.
func (o *Op) Validate() error {
	if o.key == "" {
		return errors.New("no key for OnceResult")
	}
	if o.cmd == nil {
		return errors.New("no command for OnceResult")
	}
	if v, ok := o.cmd.(Validator); ok {
		return v.Validate()
	}
	return nil
}

// Apply runs the command unless its key is recorded, and records its
// answer.
func (o *Op) Apply(tx *Tx) error {
	o.result, o.replayed = nil, false
	if err := tx.w.resultsTable(tx); err != nil {
		return err
	}
	var result []byte
	var msg sql.NullString
	err := tx.QueryRow("SELECT result, refusal FROM lith_results WHERE key = ?", o.key).Scan(&result, &msg)
	if err == nil {
		o.result, o.replayed = result, true
		if msg.Valid {
			return &Refusal{Msg: msg.String}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	applied := tx.Savepoint(func(tx *Tx) error {
		var err error
		result, err = o.cmd.ApplyResult(tx)
		return err
	})
	var refusal *Refusal
	if applied != nil && !errors.As(applied, &refusal) {
		return applied
	}
	if refusal != nil {
		msg = sql.NullString{String: refusal.Msg, Valid: true}
	}
	if _, err := tx.Exec("INSERT INTO lith_results (key, at, result, refusal) VALUES (?, ?, ?, ?)",
		o.key, time.Now().Unix(), result, msg); err != nil {
		return err
	}
	o.result = result
	if applied != nil {
		return &kept{applied}
	}
	return nil
}

func (w *Writer) resultsTable(tx *Tx) error {
	if w.resReady {
		return nil
	}
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS lith_results (key TEXT PRIMARY KEY, at INTEGER NOT NULL, result BLOB, refusal TEXT) STRICT, WITHOUT ROWID"); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS lith_results_at ON lith_results (at)"); err != nil {
		return err
	}
	tx.AfterCommit(func() { w.resReady = true })
	return nil
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
