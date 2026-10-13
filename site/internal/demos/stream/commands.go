package stream

import (
	"errors"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// SetPayload changes a tab's payload: its size, its churn, or both. Going
// still freezes the table at the step of the change.
type SetPayload struct {
	Viewer demo.Viewer
	Op     string
	Size   Size  // "" keeps the size
	Churn  Churn // "" keeps the churn
	At     time.Time
}

func (c SetPayload) Validate() error {
	switch {
	case c.Viewer.Tab == "" || c.Op == "":
		return errors.New("a change needs a tab and an op")
	case c.Size == "" && c.Churn == "":
		return errors.New("a change needs a size or a churn")
	case c.Size != "" && !c.Size.valid():
		return errors.New("the size is small, medium or large")
	case c.Churn != "" && !c.Churn.valid():
		return errors.New("the churn is still, calm, busy or storm")
	}
	return nil
}

func (c SetPayload) Apply(tx *writer.Tx) error {
	var s TabState
	if err := demo.LoadTab(tx.Context(), tx, Slug, c.Viewer, &s); err != nil {
		return err
	}
	if c.Size != "" {
		s.Size = c.Size
	}
	if c.Churn == Still && s.churn() != Still {
		s.Frozen, s.From = StepAt(c.At), s.churn()
	}
	if c.Churn != "" {
		s.Churn = c.Churn
	}
	s.Op = c.Op
	return demo.SaveTab(tx, Slug, c.Viewer, s, c.At)
}
