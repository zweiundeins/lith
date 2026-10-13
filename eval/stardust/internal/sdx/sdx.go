// Package sdx holds the Stardust plumbing the eval commands share.
package sdx

import (
	"fmt"
	"os"
	"strings"

	"stardust"
)

// Activate licenses the process with the token in path. The token is never printed.
func Activate(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := stardust.Activate(strings.TrimSpace(string(raw))); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	return nil
}

// LicenseSummary describes the process licence without its identity fields.
func LicenseSummary() string {
	l, err := stardust.License()
	if err != nil {
		return "license: " + err.Error()
	}
	return fmt.Sprintf("active=%v failure=%q tier=%q build=%s support_end=%s expires=%s",
		l.Active, l.Failure, l.Tier, day(l.Build), day(l.SupportEnd), day(l.Expires))
}

func day(t interface {
	IsZero() bool
	Format(string) string
}) string {
	if t.IsZero() {
		return "none"
	}
	return t.Format("2006-01-02T15:04Z07:00")
}

// Doc parses DUST text and panics on a syntax error: the texts are constants.
func Doc(text string) *stardust.Document {
	d, err := stardust.Parse([]byte(text), stardust.DUST)
	if err != nil {
		panic(fmt.Sprintf("parse %q: %v", text, err))
	}
	return d
}

// Text serializes a document as DUST, for printing.
func Text(d *stardust.Document) string {
	b, err := d.Serialize(stardust.DUST, 0)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}
