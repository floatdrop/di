import type { ReactNode } from 'react';

/** The two locales the site is prerendered in. */
export const LOCALES = ['en', 'ru'] as const;
export type Locale = (typeof LOCALES)[number];

/** English lives at the root, so its path segment is empty. */
export const localePath: Record<Locale, string> = { en: '', ru: 'ru/' };

/** How many directories below the build root a locale's page sits. */
export const localeDepth = (locale: Locale) => localePath[locale].split('/').length - 1;

/**
 * The code blocks, ready to drop into a section. They are the same in both
 * locales -- Go source, and output the Go tests pin -- so a translation never
 * touches them, and a section reaches the ones it shows by name.
 */
export type FigureId =
	| 'tree'
	| 'storage'
	| 'config'
	| 'main'
	| 'mail'
	| 'api'
	| 'cache'
	| 'storageTest'
	| 'cacheTest'
	| 'explain'
	| 'modules'
	| 'run';

export type Figures = Record<FigureId, ReactNode>;

/** The ten steps, in order; the id is the anchor. */
export type StepId =
	| 'shape'
	| 'constructors'
	| 'config'
	| 'main'
	| 'workers'
	| 'http'
	| 'wrap'
	| 'testing'
	| 'graph'
	| 'run';

export interface Step {
	id: StepId;
	/** Shown in the table of contents and as the heading. */
	title: string;
	/** Prose with the figures interleaved where the text puts them. */
	body: (figures: Figures) => ReactNode;
}

/**
 * Everything on the page that is words. A locale is one value of this type,
 * so a missing translation is a compile error rather than a gap on the page.
 */
export interface Content {
	locale: Locale;
	/** `lang` on the html element, and what a screen reader announces in. */
	htmlLang: string;
	/** The label of this locale in the language switch, in that locale. */
	nativeName: string;

	meta: { title: string; description: string };

	hero: {
		title: string;
		lead: ReactNode;
		/** The install line, and what the copy button puts on the clipboard. */
		install: string;
	};

	labels: {
		steps: string;
		language: string;
		copy: string;
		toLight: string;
		toDark: string;
	};

	intro: ReactNode;

	/** The annotated directory listing in step 1; paths are not translated. */
	tree: { path: string; comment: string }[];

	steps: Step[];

	footer: ReactNode;
}
