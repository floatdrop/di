import Check from '@gravity-ui/icons/Check';
import Copy from '@gravity-ui/icons/Copy';
import { Button, Icon, Text, ThemeProvider, Toc } from './uikit.ts';

import type { Code } from './code.ts';
import { Figure, Highlighted, Plain } from './components/Figure.tsx';
import { Logo, LogoSprite } from './components/Logo.tsx';
import { Topbar } from './components/Topbar.tsx';
import type { Content, Figures, Locale } from './content/types.ts';

/**
 * The figures are the same in both locales -- Go source, and output the Go
 * tests pin -- except for the annotated tree, whose comments are prose.
 */
function buildFigures(code: Code, content: Content): Figures {
	const pad = Math.max(...content.tree.map((row) => row.path.length)) + 2;

	return {
		tree: (
			<Figure caption="examples/guide">
				<Plain>
					{content.tree.map((row) => (
						<span key={row.path}>
							{row.path.padEnd(pad)}
							<span className="di-figure__comment">{row.comment}</span>
							{'\n'}
						</span>
					))}
				</Plain>
			</Figure>
		),
		storage: (
			<Figure caption="internal/storage/storage.go">
				<Highlighted html={code.html.storage} />
			</Figure>
		),
		config: (
			<Figure caption="internal/config/config.go">
				<Highlighted html={code.html.config} />
			</Figure>
		),
		main: (
			<Figure caption="cmd/api/main.go">
				<Highlighted html={code.html.main} />
			</Figure>
		),
		mail: (
			<Figure caption="internal/mail/mail.go">
				<Highlighted html={code.html.mail} />
			</Figure>
		),
		api: (
			<Figure caption="internal/api/api.go">
				<Highlighted html={code.html.api} />
			</Figure>
		),
		cache: (
			<Figure caption="internal/cache/cache.go">
				<Highlighted html={code.html.cache} />
			</Figure>
		),
		storageTest: (
			<Figure caption="internal/storage/storage_test.go">
				<Highlighted html={code.html.storageTest} />
			</Figure>
		),
		cacheTest: (
			<Figure caption="internal/cache/cache_test.go">
				<Highlighted html={code.html.cacheTest} />
			</Figure>
		),
		explain: (
			<Figure caption="app.Explain[storage.Store]()">
				<Plain>{code.explain}</Plain>
			</Figure>
		),
		modules: (
			<Figure caption="app.Modules()">
				<Plain>{code.modules}</Plain>
			</Figure>
		),
		run: (
			<Figure caption="shell">
				<Highlighted html={code.html.run} />
			</Figure>
		)
	};
}

export interface AppProps {
	content: Content;
	code: Code;
	/** Every locale's name in its own language, for the language switch. */
	names: Record<Locale, string>;
}

export function App({ content, code, names }: AppProps) {
	const figures = buildFigures(code, content);
	const { hero, labels, steps } = content;

	return (
		<ThemeProvider theme="light" lang={content.locale}>
			<LogoSprite />
			<Topbar content={content} names={names} />

			<header className="di-page di-hero">
				<div className="di-hero__copy">
					<Text as="h1" variant="display-3" className="di-hero__title">
						{hero.title}
					</Text>
					<Text as="p" variant="body-3" color="secondary" className="di-hero__lead">
						{hero.lead}
					</Text>
					<div className="di-hero__install">
						<code className="di-hero__install-code">{hero.install}</code>
						{/* Driven by the inlined script; the icons swap on a class. */}
						<Button id="di-copy" className="di-copy" view="flat" size="s" aria-label={labels.copy}>
							<Button.Icon>
								<span className="di-copy-icon di-copy-icon_idle">
									<Icon data={Copy} size={16} />
								</span>
								<span className="di-copy-icon di-copy-icon_done">
									<Icon data={Check} size={16} />
								</span>
							</Button.Icon>
						</Button>
					</div>
				</div>
				<Logo frame="full" className="di-hero__art" />
			</header>

			<div className="di-page di-layout">
				<aside className="di-toc" aria-label={labels.steps}>
					<Toc
						items={steps.map((step) => ({
							value: step.id,
							href: `#${step.id}`,
							content: step.title
						}))}
					/>
				</aside>

				<main className="di-main di-prose">
					<p className="di-intro">{content.intro}</p>

					{steps.map((step, i) => (
						<section key={step.id} id={step.id} className="di-section">
							<Text as="h2" variant="header-2" className="di-section__heading">
								<span className="di-section__number" aria-hidden="true">
									{i + 1}
								</span>
								{step.title}
							</Text>
							{step.body(figures)}
						</section>
					))}
				</main>
			</div>

			<footer className="di-footer">
				<div className="di-page">
					<Text as="p" variant="body-1" color="secondary">
						{content.footer}
					</Text>
				</div>
			</footer>
		</ThemeProvider>
	);
}
