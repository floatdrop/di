import { renderToStaticMarkup } from 'react-dom/server';

import { App } from './App.tsx';
import { loadCode } from './code.ts';
import { url } from './config.ts';
import { content, nativeNames } from './content/index.ts';
import type { Locale } from './content/types.ts';
import { LOCALES, localeDepth, localePath } from './content/types.ts';
import { inlineScript } from './inline-script.ts';
import {
	COPY_BUTTON,
	COPY_DONE,
	LANG_MENU,
	GITHUB_BUTTON,
	SECTION,
	STARS_CLASS,
	STARS_COUNT,
	THEME_TOGGLE,
	TOC_ACTIVE,
	TOC_LINK
} from './selectors.ts';

// Importing the stylesheet here is what puts it in the build: every Gravity UI
// component imports its own CSS, so the server bundle's CSS is exactly the CSS
// the rendered markup needs, and `ssrEmitAssets` writes it out.
import './styles/main.css';

const escapeAttribute = (value: string) =>
	value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');

/** Safe to drop inside a <script> element: no `</script>` can come out. */
const literal = (value: unknown) => JSON.stringify(value).replace(/</g, '\\u003c');

export interface Page {
	/** Where the file goes, relative to the build directory. */
	file: string;
	html: string;
}

/** How the page gets its CSS, which is the one thing dev and the build differ on. */
export type Styles =
	/** The stylesheet the build emitted, relative to the build directory. */
	| { link: string }
	/** A module that pulls the same CSS in as a side effect; see src/dev-styles.ts. */
	| { module: string };

/** One prerendered page per locale. */
export async function render(styles: Styles): Promise<Page[]> {
	const code = await loadCode();

	return LOCALES.map((locale) => ({
		file: `${localePath[locale]}index.html`,
		html: page(
			locale,
			styles,
			renderToStaticMarkup(<App content={content[locale]} code={code} names={nativeNames} />)
		)
	}));
}

function page(locale: Locale, styles: Styles, body: string): string {
	const { hero, htmlLang, labels, meta } = content[locale];
	const depth = localeDepth(locale);

	// Runs before the body is parsed, so the theme is settled before the first
	// paint. See src/inline-script.ts for why it is inlined this way.
	const script = `(${inlineScript.toString()})(${literal({
		themeToggle: THEME_TOGGLE,
		copyButton: COPY_BUTTON,
		copyDone: COPY_DONE,
		langMenu: LANG_MENU,
		section: SECTION,
		tocLink: TOC_LINK,
		tocActive: TOC_ACTIVE,
		// Below the sticky topbar, and below `scroll-margin-top` as well: a
		// section jumped to from the contents lands exactly on its scroll
		// margin, and a line at that same height makes it a coin toss whether
		// the section you just jumped to counts as reached.
		tocOffset: 88,
		githubButton: GITHUB_BUTTON,
		starsCount: STARS_COUNT,
		starsClass: STARS_CLASS,
		starsApi: 'https://api.github.com/repos/floatdrop/di',
		copyText: hero.install,
		labels: { toLight: labels.toLight, toDark: labels.toDark }
	})})`;

	const alternates = LOCALES.map(
		(l) => `<link rel="alternate" hreflang="${l}" href="${url(localePath[l], depth)}" />`
	).join('\n\t\t');

	const stylesheet =
		'link' in styles
			? `<link rel="stylesheet" href="${url(styles.link, depth)}" />`
			: `<script type="module" src="${styles.module}"></script>`;

	return `<!doctype html>
<html lang="${htmlLang}" class="g-root g-root_theme_light">
	<head>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width, initial-scale=1" />
		<title>${escapeAttribute(meta.title)}</title>
		<meta name="description" content="${escapeAttribute(meta.description)}" />
		<link rel="icon" href="${url('favicon.svg', depth)}" />
		${alternates}
		<link rel="alternate" hreflang="x-default" href="${url(localePath.en, depth)}" />
		${stylesheet}
		<script>${script}</script>
	</head>
	<body>${body}</body>
</html>
`;
}
