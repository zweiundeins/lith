---
title: events/eventstest
headline: lith/events/eventstest: deciders as given, when, then
summary: Tests deciders as given, when, then: the events a stream holds, a command, and the events or the error the command should cause.
description: Reference for lith/events/eventstest: test a decider with the events a stream holds, a command, and the events or error it causes, and check samples.
order: 4
---

Import path `github.com/zweiundeins/lith/events/eventstest`. Generated from the doc comments in [events/eventstest/](https://github.com/zweiundeins/lith/tree/main/events/eventstest).

It runs the fold and the decision without a database.

```go
func TestRename(t *testing.T) {
	eventstest.For(rename).
		Given(CardCreated{Title: "Draft"}).
		When(Rename{Title: "Final"}).
		Then(t, CardRenamed{From: "Draft", To: "Final"})
}
```

## Functions

### func Samples {#Samples}

```go
func Samples[K events.Key, E any](t testing.TB, st *events.Store, s *events.Stream[K, E], fsys fs.FS)
```

Samples checks the JSON samples of a stream's event types in fsys, a file per type named after it, such as card.renamed.json: every type of the stream has one, and each decodes, without a field its type lacks, and upcasts (events.CheckSample). Keep a sample of every type as it was first stored, and never change one: a field renamed instead of upcast then fails the test instead of the reads in production.

## Types

### type Case {#Case}

```go
type Case[C, E, S any] struct {
	// contains filtered or unexported fields
}
```

Case is a decider's test case. Given and When return a new Case, so a case can serve as the start of several.

#### func For {#For}

```go
func For[C, E, S any](d events.Decider[C, E, S]) Case[C, E, S]
```

For starts a test case of decider d.

#### func (Case[C, E, S]) Given {#Case.Given}

```go
func (c Case[C, E, S]) Given(evs ...E) Case[C, E, S]
```

Given adds events that the stream holds before the command.

#### func (Case[C, E, S]) State {#Case.State}

```go
func (c Case[C, E, S]) State() S
```

State returns the state folded from the given events.

#### func (Case[C, E, S]) Then {#Case.Then}

```go
func (c Case[C, E, S]) Then(t testing.TB, want ...E)
```

Then checks that the command causes exactly the events want, in order, and no error. Then(t) checks that it causes nothing.

#### func (Case[C, E, S]) ThenError {#Case.ThenError}

```go
func (c Case[C, E, S]) ThenError(t testing.TB, want error)
```

ThenError checks that the command fails with want: an error that errors.Is matches, or one with the same message, such as a \*writer.Refusal.

#### func (Case[C, E, S]) When {#Case.When}

```go
func (c Case[C, E, S]) When(cmd C) Case[C, E, S]
```

When sets the command.
