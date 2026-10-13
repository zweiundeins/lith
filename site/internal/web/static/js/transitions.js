// transitions.js: the site's one script of its own. It loads in <head>
// before the first frame, so that its pagereveal listener runs on every
// page. It gives each cross-document view transition its type, names the
// elements that morph from one page into the next, keeps the docs
// navigation's scroll position, and runs the display menu (theme, corners)
// with a circle that opens from its button. Without view transitions, or
// with reduced motion, pages and settings change at once.
;(() => {
	'use strict'
	const root = document.documentElement
	const reduced = matchMedia('(prefers-reduced-motion: reduce)')

	const pathOf = (url) => {
		try {
			return new URL(url, location.href).pathname
		} catch {
			return ''
		}
	}
	const demoOf = (path) => /^\/demos\/([a-z0-9-]+)\/?$/.exec(path)?.[1]
	const isDocs = (path) => path === '/docs' || path.startsWith('/docs/')

	// The type of a move from one page to another (transitions.css).
	const typeOf = (from, to) => {
		if (!from || from === to) return 'fade'
		if (isDocs(from) && isDocs(to)) {
			const order = Array.from(document.querySelectorAll('[data-docs-nav] a'), (a) => a.pathname)
			const a = order.indexOf(from)
			const b = order.indexOf(to)
			if (a >= 0 && b >= 0) return b > a ? 'forward' : 'backward'
			return 'fade'
		}
		if (from === '/') return 'zoom-in'
		if (to === '/') return 'zoom-out'
		if (from === '/demos' && demoOf(to)) return 'zoom-in'
		if (demoOf(from) && to === '/demos') return 'zoom-out'
		return 'fade'
	}

	// Names given for one transition, taken back when it is over.
	const named = []
	const name = (el, n) => {
		if (!el) return
		el.style.viewTransitionName = n
		named.push(el)
	}
	const unname = () => {
		for (const el of named.splice(0)) el.style.viewTransitionName = ''
	}

	// A demo's card on this page (the landing page, /demos) takes the names
	// of the head of the demo's page: the card, its title and its art.
	const nameCard = (slug) => {
		const card = document.querySelector(`[data-demo-card="${slug}"]`)
		if (!card) return false
		name(card, `demo-${slug}`)
		name(card.querySelector('h3'), `demo-${slug}-title`)
		name(card.querySelector('.demo-card__art'), `demo-${slug}-art`)
		return true
	}

	const navScroll = 'lith-docs-nav-scroll'

	addEventListener('pageswap', (e) => {
		const nav = document.querySelector('[data-docs-nav]')
		if (nav) sessionStorage.setItem(navScroll, String(nav.scrollTop))
		if (!e.viewTransition || !e.activation?.entry) return
		const from = location.pathname
		const to = pathOf(e.activation.entry.url)
		const slug = demoOf(to)
		if (slug) nameCard(slug)
		// A docs page chosen in the navigation: its link turns into the next
		// page's title, and this page's title steps aside.
		if (isDocs(from) && isDocs(to) && e.activation.navigationType !== 'traverse') {
			const link = Array.from(document.querySelectorAll('[data-docs-nav] a')).find((a) => a.pathname === to)
			const title = document.querySelector('.page-title')
			if (link && title && link.getAttribute('aria-current') !== 'page' && link.checkVisibility?.()) {
				name(title, 'none')
				name(link.querySelector('span'), 'page-title')
			}
		}
	})

	// Back on a page from the cache: it shows as it was left.
	addEventListener('pageshow', (e) => {
		if (e.persisted) unname()
	})

	addEventListener('pagereveal', (e) => {
		const nav = document.querySelector('[data-docs-nav]')
		const top = sessionStorage.getItem(navScroll)
		if (nav && top !== null) nav.scrollTop = Number(top)
		if (!e.viewTransition) return
		if (reduced.matches) {
			e.viewTransition.skipTransition()
			return
		}
		const from = pathOf(window.navigation?.activation?.from?.url ?? document.referrer)
		const to = location.pathname
		const type = typeOf(from, to)
		e.viewTransition.types?.add(type)
		root.dataset.lithTransition = type // the type of the transition that brought the page, for tests
		// Back from a demo to a page with its card: the card takes the names
		// the demo's head had, and the head shrinks back into it.
		const slug = demoOf(from)
		if (slug && !demoOf(to) && nameCard(slug)) e.viewTransition.finished.finally(unname)
	})

	// The display menu: data-pref names the setting (theme, style) and
	// data-value the choice. A choice sets data-lith-<pref> on <html>, keeps
	// it in the cookie lith-<pref> that the server reads, and the new look
	// opens as a circle from the menu's button.
	const typed = typeof ViewTransition !== 'undefined' && 'types' in ViewTransition.prototype

	const choose = (key, value) => {
		const attr = 'lith' + key[0].toUpperCase() + key.slice(1)
		const apply = () => {
			root.dataset[attr] = value
			document.cookie = `lith-${key}=${value}; Path=/; Max-Age=34560000; SameSite=Lax`
			for (const b of document.querySelectorAll(`[data-pref="${key}"]`)) {
				b.setAttribute('aria-pressed', String(b.dataset.value === value))
			}
			if (key === 'theme') {
				document.querySelector('meta[name="color-scheme"]')?.setAttribute('content', value === 'system' ? 'light dark' : value)
			}
		}
		if (root.dataset[attr] === value) return
		if (!document.startViewTransition || reduced.matches) {
			apply()
			return
		}
		const r = document.querySelector('.prefs-button')?.getBoundingClientRect()
		const x = r ? r.left + r.width / 2 : innerWidth
		const y = r ? r.top + r.height / 2 : 0
		const radius = Math.hypot(Math.max(x, innerWidth - x), Math.max(y, innerHeight - y))
		root.dataset.vt = 'theme'
		const t = typed ? document.startViewTransition({ update: apply, types: ['theme'] }) : document.startViewTransition(apply)
		t.ready
			.then(() => {
				root.animate(
					{ clipPath: [`circle(0px at ${x}px ${y}px)`, `circle(${radius}px at ${x}px ${y}px)`] },
					{ duration: 640, easing: 'cubic-bezier(0.65, 0, 0.35, 1)', pseudoElement: '::view-transition-new(root)' },
				)
			})
			.catch(() => {})
		t.finished.finally(() => delete root.dataset.vt)
	}

	document.addEventListener('click', (e) => {
		const b = e.target instanceof Element && e.target.closest('[data-pref]')
		if (b) choose(b.dataset.pref, b.dataset.value)
	})
})()
