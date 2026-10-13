---
title: Guides
nav: Recipes
summary: The smallest code for each job in a lith app, from a command with an op id to a bulk import.
order: 3
draft: true
---

This section is not written yet. It gets one page per recipe in `skills/lith/references/recipes.md`, each with code that compiles against the current lith:

- op ids that keep refusals (`writer.Once`, `writer.OnceResult`)
- query code shared by a GET, a view and a command (`sqlite.Querier`)
- tab state and preferences in SQLite
- a background job, and a sweep for every table that grows
- a bulk import in sorted chunks
- work a transaction cannot hold (`w.Exclusive`)
- an event-sourced command
- backups, with and without Litestream
