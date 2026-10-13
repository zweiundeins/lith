## The command each producer sends

Every command of the race is a transfer, the command of lith's writer benchmark: it reads a balance, debits one account, credits another and appends to a ledger. With the writer's `SAVEPOINT` and `RELEASE` around it, that is six statements.

```go title=race.go
func (c transfer) Apply(tx *writer.Tx) error {
	var balance int64
	if err := tx.QueryRow(`SELECT balance FROM accounts WHERE id = ?`, c.from).Scan(&balance); err != nil {
		return err
	}
	if balance < c.amount {
		return errInsufficient
	}
	if _, err := tx.Exec(`UPDATE accounts SET balance = balance - ?, version = version + 1 WHERE id = ?`, c.amount, c.from); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE accounts SET balance = balance + ?, version = version + 1 WHERE id = ?`, c.amount, c.to); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ledger (from_id, to_id, amount, at) VALUES (?, ?, ?, ?)`, c.from, c.to, c.amount, time.Now().Unix()); err != nil {
		return err
	}
	tx.Touch(c.topic)
	return nil
}
```

## The producers

A producer is a goroutine that waits in `w.Do` until its command has committed, as a request handler does, and then sends the next one. While the writer runs a batch, the commands of the other producers queue up, and the next batch takes all of them. One producer gets a transaction for every command. With two hundred, a transaction takes a hundred commands or more, and they share its commit, and with `synchronous=FULL` its fsync.

```go title=race.go
for p := range int64(s.Producers) {
	wg.Go(func() {
		first := p*Accounts + 1
		topic := fmt.Sprint("producer:", p)
		for i := int64(0); time.Now().Before(deadline) && ctx.Err() == nil; i++ {
			c := transfer{from: first + i%Accounts, to: first + (i+1)%Accounts, amount: 1 + i%100, topic: topic}
			if err := w.Do(ctx, c); err != nil {
				if ctx.Err() == nil {
					once.Do(func() { failed = err })
				}
				return
			}
		}
	})
}
```

## The numbers come from writer.Stats

Every 50 ms the race reads its writer's `Stats` and counts from the moment the producers started. `Stats.Sizes` counts the transactions by the commands they took, in powers of two: the histogram on the track draws it. `P99Commit` is the 99th percentile of the last 1,024 transactions, from `BEGIN IMMEDIATE` to the end of `COMMIT`.

```go title=race.go
read := func() Sample {
	st := w.Stats()
	return Sample{
		At:       time.Since(start),
		Commands: st.Commands - base.Commands,
		Batches:  st.Batches - base.Batches,
		Failures: st.Failures - base.Failures,
		Queue:    st.Queue,
		MaxBatch: st.MaxBatch,
		P99:      st.P99Commit,
		Sizes:    sizesOf(st.Sizes).minus(sizesOf(base.Sizes)),
	}
}
```

## From the race to every tab

The race has a writer of its own on a file of its own: its commands never queue in front of the site's. Its samples reach your page the way every change on this site does, as commands on the site's writer. The runner sends each sample as a `Record` with `Send`, which returns at once, and ends the race with a `Finish`.

```go title=routes.go
func (r *runner) run(race Race) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	last, err := r.e.Run(ctx, race.ID, race.Settings, func(s Sample) {
		switch err := r.w.Send(Record{Race: race.ID, Sample: s}); {
		case errors.Is(err, writer.ErrClosed):
			cancel() // the site stops: so does the race
		case err != nil:
			r.log.Warn("writer race: a sample", "race", race.ID, "err", err)
		}
	})
```

`Record` stores the sample and touches `writer-race:live`. The track's view listens to that topic: it renders once in the next tick, from the samples in SQLite, and every tab that shows the track gets the same frame. Its frames morph at once, without a view transition; the bars and the progress ease between them in CSS.

```go title=live.go
func trackView() live.View {
	return live.View{
		Key:    Slug + ":track",
		Topics: []string{Topic, LiveTopic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			t, err := loadTrack(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Track(t))
		},
	}
}
```

## One race at a time

The line is a table on the site's database, so it survives a deploy. One goroutine, the runner, takes races off it. A `Start` that puts a race in line wakes it with `tx.AfterCommit`; it then runs races until nobody waits. `claim` is a command too: it marks the first race in line as running once it has waited 800 ms, so that every tab sees it join the line before it leaves it.

```go title=routes.go
func (r *runner) races() error {
	for {
		n, err := r.w.DoValue(context.Background(), claim(time.Now(), r.dwell))
		switch {
		case err != nil:
			return err
		case n.Wait > 0:
			time.Sleep(n.Wait) // the first in line joined a moment ago
		case n.Race.ID == 0:
			return nil // nobody waits
		default:
			r.run(n.Race)
			time.Sleep(r.pause)
		}
	}
}
```

## What a race costs this server

The race's writer is one goroutine, so a race keeps one core busy at most, and less with `FULL`, where it waits for the disk. On the Intel i5-13500 of lith's benchmarks, with 100 tabs open on this page, the whole site used about 0.9 cores while a race of 200 producers with `NORMAL` ran, and its heap grew by about 20 MiB at the peak. The race's file is deleted when it ends, and the site deletes any that a restart left behind when it starts.

The writer's goroutine stays on a thread of its own, at a lower CPU and disk priority, so the site and the other services on this server win whenever they want the CPU or the disk. The thread ends with the race.

```go title=prio_linux.go
func lowerPriority() error {
	tid := syscall.Gettid()
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, 10); err != nil {
		return err
	}
	const whoProcess, classBestEffort, classShift, lowest = 1, 2, 13, 7
	if _, _, e := syscall.Syscall(syscall.SYS_IOPRIO_SET, whoProcess, uintptr(tid), classBestEffort<<classShift|lowest); e != 0 {
		return e
	}
	return nil
}
```
