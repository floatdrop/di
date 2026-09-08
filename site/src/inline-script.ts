export interface InlineConfig {
	/** Selector for the theme button. */
	themeToggle: string;
	/** Selector for the copy button beside the install line. */
	copyButton: string;
	/** Class the copy button wears for a moment after a successful copy. */
	copyDone: string;
	/** Selector for the language menu's `details` element. */
	langMenu: string;
	/** Selector for the sections the table of contents points at. */
	section: string;
	/** Selector for the table of contents' links. */
	tocLink: string;
	/** uikit's active modifier, set on each link's parent. */
	tocActive: string;
	/** How far below the viewport top a section counts as current. */
	tocOffset: number;
	/** Selector for the GitHub button the star count is appended to. */
	githubButton: string;
	/** Selector for that count once it exists. */
	starsCount: string;
	/** Class it is created with. */
	starsClass: string;
	/** The GitHub API endpoint for this repository. */
	starsApi: string;
	/** What the copy button puts on the clipboard. */
	copyText: string;
	labels: {
		/** Announced by the theme button while the dark theme is on. */
		toLight: string;
		/** Announced while the light theme is on. */
		toDark: string;
	};
}

/**
 * The only JavaScript the page ships. It is inlined into the head by calling
 * `Function.prototype.toString` on it (see entry-server.tsx), which is why it
 * closes over nothing and takes what it needs as an argument: everything it
 * refers to has to survive being cut out of the module.
 *
 * It runs before the body is parsed, so the theme class is right before the
 * first paint and there is no flash. Nothing it drives exists yet at that
 * point, which is why every handler is delegated to the document.
 */
export function inlineScript(config: InlineConfig) {
	const KEY = 'di-theme';
	const DARK = 'g-root_theme_dark';
	const LIGHT = 'g-root_theme_light';
	const root = document.documentElement;
	const darkMedia = window.matchMedia('(prefers-color-scheme: dark)');

	/** What was chosen last, or null; storage can be denied outright. */
	function chosen(): string | null {
		try {
			return localStorage.getItem(KEY);
		} catch {
			return null;
		}
	}

	function system(): string {
		return darkMedia.matches ? 'dark' : 'light';
	}

	function apply(theme: string) {
		root.classList.toggle(DARK, theme === 'dark');
		root.classList.toggle(LIGHT, theme !== 'dark');
		const button = document.querySelector(config.themeToggle);
		if (button) {
			button.setAttribute('aria-label', theme === 'dark' ? config.labels.toLight : config.labels.toDark);
		}
	}

	function closeMenus(except: Element | null) {
		document.querySelectorAll(config.langMenu).forEach(function (menu) {
			if (menu !== except) {
				menu.removeAttribute('open');
			}
		});
	}

	/**
	 * Marks the section being read in the table of contents. uikit's Toc takes
	 * its active item from a `value` prop, and this page has no React to change
	 * one, so the class it would have set is set here instead.
	 */
	function spy() {
		const sections = document.querySelectorAll(config.section);
		// The first section stands in until one has actually passed the line, so
		// the rail is drawn on arrival rather than on the first scroll.
		let current = sections.item(0)?.id ?? null;
		sections.forEach(function (section) {
			if (section.getBoundingClientRect().top <= config.tocOffset) {
				current = section.id;
			}
		});
		// The last sections are shorter than the viewport, so the page runs out
		// of scroll before their tops reach the line and they could never be
		// current. At the bottom, the last one is what you are reading.
		if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) {
			current = sections.item(sections.length - 1)?.id ?? current;
		}
		document.querySelectorAll(config.tocLink).forEach(function (link) {
			const on = current !== null && link.getAttribute('href') === '#' + current;
			link.parentElement?.classList.toggle(config.tocActive, on);
		});
	}

	/**
	 * The star count, which is the one thing on the page that comes from
	 * somewhere else. Last visit's number goes in first so the button does not
	 * change width halfway through reading, and a failure of any kind -- denied
	 * storage, rate limit, no network, GitHub down -- leaves the button as
	 * rendered rather than showing an error nobody asked about.
	 */
	function stars() {
		const button = document.querySelector(config.githubButton);
		if (!button) {
			return;
		}

		function show(text: string) {
			let el = button!.querySelector(config.starsCount);
			if (!el) {
				el = document.createElement('span');
				el.className = config.starsClass;
				button!.appendChild(el);
			}
			el.textContent = text;
		}

		const KEY = 'di-stars';
		try {
			const cached = localStorage.getItem(KEY);
			if (cached) {
				show(cached);
			}
		} catch {
			// No storage; the fetch below is the only source then.
		}
		fetch(config.starsApi, { headers: { accept: 'application/vnd.github+json' } })
			.then(function (response) {
				return response.ok ? response.json() : null;
			})
			.then(function (repo) {
				const count: unknown = repo?.stargazers_count;
				if (typeof count !== 'number') {
					return;
				}
				const text =
					count >= 1000 ? (count / 1000).toFixed(1).replace(/\.0$/, '') + 'k' : String(count);
				show(text);
				try {
					localStorage.setItem(KEY, text);
				} catch {
					// Nothing to carry to the next visit.
				}
			})
			.catch(function () {
				// Left as rendered.
			});
	}

	apply(chosen() ?? system());

	// The first call settles the theme before anything paints, which is the
	// point of running in the head -- but the button it labels is parsed after
	// it, so the label is set again once there is something to set it on. The
	// same goes for everything else here: none of it exists yet.
	function ready() {
		apply(root.classList.contains(DARK) ? 'dark' : 'light');

		stars();

		let queued = false;
		spy();
		document.addEventListener(
			'scroll',
			function () {
				if (queued) {
					return;
				}
				queued = true;
				requestAnimationFrame(function () {
					queued = false;
					spy();
				});
			},
			{ passive: true }
		);
	}

	if (document.readyState === 'loading') {
		document.addEventListener('DOMContentLoaded', ready);
	} else {
		ready();
	}

	document.addEventListener('click', function (event) {
		const target = event.target;
		if (!(target instanceof Element)) {
			return;
		}

		if (target.closest(config.themeToggle)) {
			const next = root.classList.contains(DARK) ? 'light' : 'dark';
			try {
				localStorage.setItem(KEY, next);
			} catch {
				// Storage is denied; the choice holds for this page only.
			}
			apply(next);
			return;
		}

		const copy = target.closest(config.copyButton);
		if (copy && navigator.clipboard) {
			navigator.clipboard.writeText(config.copyText).then(
				function () {
					copy.classList.add(config.copyDone);
					setTimeout(function () {
						copy.classList.remove(config.copyDone);
					}, 1600);
				},
				function () {
					// Denied. Nothing to say about it that the page can say well.
				}
			);
			return;
		}

		// `details` opens and closes itself, but nothing dismisses it when the
		// click lands somewhere else.
		closeMenus(target.closest(config.langMenu));
	});

	document.addEventListener('keydown', function (event) {
		if (event.key === 'Escape') {
			closeMenus(null);
		}
	});

	// Someone who never used the button keeps following the system.
	darkMedia.addEventListener('change', function () {
		if (!chosen()) {
			apply(system());
		}
	});
}
