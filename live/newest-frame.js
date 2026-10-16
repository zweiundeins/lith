// newest-frame.js is a Datastar plugin: with data-newest-frame on <body>,
// the page applies only the newest frame of each view of its stream (the
// element #stream, or the id after a colon). It holds the stream's patch
// events and sends them on once a chunk is parsed, or at the next animation
// frame when it sent a batch less than a frame ago, and no sooner after a
// batch than half the time that batch took to apply; a patch that a newer
// one in the batch replaces wholly is never applied. A page that keeps up
// applies every frame. When more than half of the last 3 s of frames were
// skipped, at least 10, it sets $_slow and opens the stream again at the
// same URL, which then sends Save-Data: on. See live.NewestFrame.
import { attribute, getPath, mergePaths } from 'datastar'

const SPAN = 3000 // ms of frames the share counts
const MIN = 10 // frames in SPAN, at least
const SHARE = 0.5 // of them skipped, more than
const REST = 0.5 // of the time a batch took to apply, before the next
const ELEMENTS = 'datastar-patch-elements'
const SIGNALS = 'datastar-patch-signals'

attribute({
  name: 'newest-frame',
  requirement: { key: 'allowed', value: 'denied' },
  apply({ key }) {
    const stream = key || 'stream'
    const replay = new WeakSet()
    let queue = []
    let held = false // a flush is queued for this chunk
    let frame = 0 // the animation frame the next batch waits for
    let sentAt = 0
    let took = 0 // ms the last batch took to apply
    let timer = 0
    let asked = false // it set $_slow
    const recent = [] // [time, frames, skipped]

    const send = (d) => {
      replay.add(d)
      document.dispatchEvent(new CustomEvent('datastar-fetch', { detail: d }))
    }

    // flush sends the batch on, unless it waits: for an animation frame,
    // or until half the time the last batch took to apply has passed since.
    // Firefox hands a page that fell behind one frame per task while its
    // main thread is free, so that wait lets them gather.
    const flush = () => {
      held = false
      if (!queue.length) return
      // A hidden page paints nothing, and a frame that does not come within
      // a second (a throttled iframe) holds nothing back.
      const now = performance.now()
      if (!document.hidden && now - sentAt < 1000) {
        const rest = sentAt + took * REST - now
        if (rest > 0) {
          timer ||= setTimeout(onTimer, rest)
          return
        }
        if (frame) return
      }
      const batch = queue
      queue = []
      const seen = new Set()
      let skipped = 0
      for (let i = batch.length - 1; i >= 0; i--) {
        const q = batch[i]
        if (!q.key) continue
        if (seen.has(q.key)) {
          q.drop = true
          skipped++
        } else {
          seen.add(q.key)
        }
      }
      const start = performance.now()
      for (const q of batch) {
        if (!q.drop && q.d.el.isConnected) send(q.d)
      }
      sentAt = performance.now()
      took = sentAt - start
      if (skipped) count(sentAt, 0, skipped)
      frame ||= requestAnimationFrame(onFrame)
    }
    const onFrame = () => {
      frame = 0
      flush()
    }
    const onTimer = () => {
      timer = 0
      flush()
    }

    const onFetch = (e) => {
      const d = e.detail
      if (!d || replay.has(d) || d.el?.id !== stream) return
      if (d.type !== ELEMENTS && d.type !== SIGNALS) return
      e.stopImmediatePropagation()
      // A stream that was opened again still delivers what the browser
      // holds of the old one.
      if (!d.el.isConnected) return
      let key = '' // signal patches keep their place and are never dropped
      if (d.type === ELEMENTS) {
        key = wholeTarget(d.argsRaw || {})
        count(performance.now(), 1, 0)
      }
      queue.push({ d, key })
      if (!held) {
        held = true
        queueMicrotask(flush)
      }
    }

    // count notes frames and how many of them were skipped, and asks for a
    // slow stream once more than half of the last 3 s were, at least 10.
    const count = (t, frames, skipped) => {
      if (asked || getPath('_slow')) return
      recent.push([t, frames, skipped])
      while (recent[0][0] < t - SPAN) recent.shift()
      if (recent[0][0] > t - SPAN * 0.9) return
      let all = 0
      let gone = 0
      for (const r of recent) {
        all += r[1]
        gone += r[2]
      }
      if (all >= MIN && gone / all > SHARE) toSlow()
    }

    // toSlow sets $_slow and opens the stream again from a fresh copy of
    // its element: the URL stays the same, so Datastar aborts the old
    // request, and the new one sends Save-Data: on.
    const toSlow = () => {
      asked = true
      mergePaths([['_slow', true]])
      const el = document.getElementById(stream)
      if (el) el.replaceWith(el.cloneNode(true))
    }

    addEventListener('datastar-fetch', onFetch, true)
    document.addEventListener('visibilitychange', flush)
    return () => {
      removeEventListener('datastar-fetch', onFetch, true)
      document.removeEventListener('visibilitychange', flush)
      if (frame) cancelAnimationFrame(frame)
      clearTimeout(timer)
      frame = timer = 0
      queue = []
    }
  },
})

// wholeTarget names what a patch replaces wholly, or returns '' when it may
// do more: a patch in mode outer or replace of the ids of its top-level
// elements (with no selector, or with the selector of that one id), or in
// mode inner of an id selector. One with a script or a view transition is
// never dropped.
const wholeTarget = (args) => {
  const { selector, mode = 'outer', namespace = 'html', elements } = args
  if (typeof elements !== 'string' || /<script[\s/>]/i.test(elements) || args.useViewTransition === 'true') {
    return ''
  }
  const id = typeof selector === 'string' && selector ? /^#([\w-]+)$/.exec(selector)?.[1] : undefined
  if (mode === 'inner') return id ? `inner ${namespace} ${id}` : ''
  if (mode !== 'outer' && mode !== 'replace') return ''
  const ids = topIds(elements)
  return ids && (!selector || ids === id) ? `${mode} ${namespace} ${ids}` : ''
}

const VOID_TAG = /^(?:area|base|br|col|embed|hr|img|input|link|meta|source|track|wbr)$/i
const DOCUMENT_TAG = /^(?:html|head|body)$/i
const TAG = /<(?:!--[\s\S]*?-->|(\/?)([a-z][^\s/>]*)((?:\s+[^\s"'>/=]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*)\s*(\/?)>)/gi
const ID_ATTRIBUTE = /\sid\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+))/i

// topIds returns the ids of the top-level elements, sorted, or '' when one
// has none or its end tag is missing. Inside an element it counts only the
// tags of its name, so inner elements may leave out their end tags.
const topIds = (html) => {
  const ids = []
  let open = ''
  let depth = 0
  for (const [, close, tag, attributes, selfClosing] of html.matchAll(TAG)) {
    if (!tag) continue
    const name = tag.toLowerCase()
    if (depth) {
      if (name === open && !selfClosing) depth += close ? -1 : 1
      continue
    }
    if (close) return ''
    let id = `<${name}>`
    if (!DOCUMENT_TAG.test(name)) {
      const m = ID_ATTRIBUTE.exec(attributes)
      id = m ? (m[1] ?? m[2] ?? m[3]) : ''
    }
    if (!id) return ''
    ids.push(id)
    if (!selfClosing && !VOID_TAG.test(name)) {
      open = name
      depth = 1
    }
  }
  return depth ? '' : ids.sort().join(' ')
}
