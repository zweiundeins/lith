package main

// Live mutations under test. laneCount keeps a per-lane card count on four
// lane entities; doneAt stamps a card with the time the writer put
// into the #_tx metadata of the transaction that moved it to the last lane
// (transaction entities do not expose stardust/committedAt to queries).
const laneCountText = `live    true
query   {find    [?l [count ?c ?n]]
         where   [[?l lindex ?i]
                  [?c lane ?i]]
         groupBy [?l]}
patch   {?l {cards ?n}}`

const doneAtText = `live    true
query   {find  [?c ?at]
         where [[?c lane 3 ?tx]
                [?tx at ?at]]}
patch   {?c {doneAt ?at}}`

// conflictA and conflictB write different values to one path with equal
// priority, which fails a round with Reactor_Conflict.
const conflictAText = `live    true
query   {find  [?l]
         where [[?l lindex 0]]}
patch   {?l {label a}}`

const conflictBText = `live    true
query   {find  [?l]
         where [[?l lindex 0]]}
patch   {?l {label b}}`

const countsText = `find    [?i ?n]
where   [[?l lindex ?i]
         [?l cards ?n]]
orderBy [?i]`

const doneText = `find  [?c ?at]
where [[?c doneAt ?at]]`
