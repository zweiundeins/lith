---
title: Concepts
nav: The four ideas
headline: The four ideas of lith: writes, views, ticks, HTML
summary: Linear writes, Immediate mode, Ticks, Hypermedia: what each idea means, and the package that keeps it.
description: Linear writes, Immediate mode, Ticks and Hypermedia: what each of lith's four ideas means, how a click travels through them, and the package behind each.
order: 2
---

lith's name lists its design. Each idea is a rule that an app keeps, and a package that makes the rule cheap to keep.

| Idea | The rule | Package |
| --- | --- | --- |
| [Linear writes](/docs/concepts/linear-writes) | Only the writer writes: one command after another, in batches. | `writer` |
| [Immediate mode](/docs/concepts/immediate-mode) | A region of a page is a function of the database, rendered whole. | `live`, templ |
| [Ticks](/docs/concepts/ticks) | Commits mark views dirty, and each dirty view renders once per tick. | `live` |
| [Hypermedia](/docs/concepts/hypermedia) | Frames go out as Datastar events over SSE, each stream compressed on its own. | `sse`, `edge` |

## The way of a click

1. The browser posts an action, such as `@post('/cmd/sign')`.
2. The handler validates it, hands a command to the writer with `Do`, and answers 204 once the command has committed.
3. The writer applies the command in a batch with the others that arrived, and passes the topics the batch touched to the hub.
4. The hub marks the views of those topics dirty. In the next tick, a worker renders each of them once, in one read transaction.
5. Every stream that watches one of those views gets its new frame, compressed with the stream's own window, and Datastar morphs it into the page.

The answer to the click carries nothing. What the click changed reaches the page the way every other change does: on its stream, a tick later, and on every other page that shows it at the same moment.
