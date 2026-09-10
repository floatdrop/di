// One icon per import rather than the package's barrel, which reaches every
// icon there is and would be tree-shaken back down to these five.
import ChevronDown from '@gravity-ui/icons/ChevronDown';
import Globe from '@gravity-ui/icons/Globe';
import LogoGithub from '@gravity-ui/icons/LogoGithub';
import Moon from '@gravity-ui/icons/Moon';
import Sun from '@gravity-ui/icons/Sun';
import { Button, Icon, Menu } from '../uikit.ts';

import { REPO, url } from '../config.ts';
import type { Content, Locale } from '../content/types.ts';
import { LOCALES, localeDepth, localePath } from '../content/types.ts';
import { Logo } from './Logo.tsx';

interface TopbarProps {
	content: Content;
	/** Every locale's name in its own language. */
	names: Record<Locale, string>;
}

export function Topbar({ content, names }: TopbarProps) {
	const { labels, locale } = content;
	const depth = localeDepth(locale);

	return (
		<div className="di-topbar">
			<div className="di-page di-topbar__inner">
				<a className="di-topbar__brand" href={url(localePath[locale], depth)}>
					<Logo frame="face" className="di-topbar__mark" />
					floatdrop/di
				</a>

				<div className="di-controls">
					{/* A `details`, so it opens with no JavaScript; the inlined script
					    only dismisses it when the next click lands elsewhere. */}
					<details className="di-lang">
						<Button component="summary" className="di-lang__toggle" view="flat" size="m">
							<Button.Icon side="start">
								<Icon data={Globe} size={16} />
							</Button.Icon>
							{names[locale]}
							<Button.Icon side="end">
								<Icon data={ChevronDown} size={16} />
							</Button.Icon>
						</Button>
						<div className="di-lang__panel">
							<Menu size="m" aria-label={labels.language}>
								{LOCALES.map((l) => (
									<Menu.Item
										key={l}
										href={url(localePath[l], depth)}
										active={l === locale}
										extraProps={{ lang: l, hrefLang: l }}
									>
										{names[l]}
									</Menu.Item>
								))}
							</Menu>
						</div>
					</details>

					{/* Square against the round one beside it, and both filled rather
					    than outlined: an outlined pair reads as two boxes before it
					    reads as two buttons. Labelled by the inlined script. */}
					<Button id="di-theme" view="normal" size="m" aria-label={labels.toDark}>
						<Button.Icon>
							<span className="di-theme-icon di-theme-icon_on-light">
								<Icon data={Moon} size={16} />
							</span>
							<span className="di-theme-icon di-theme-icon_on-dark">
								<Icon data={Sun} size={16} />
							</span>
						</Button.Icon>
					</Button>

					{/* Rendered as the bare mark. The inlined script appends the star
					    count if GitHub answers, and uikit's own `:has(:only-child)`
					    rule then stops treating the button as icon-only and opens it
					    into a pill. With no JavaScript, no network or no answer, the
					    markup is exactly this and the circle is what it was. */}
					<Button
						id="di-github"
						view="normal"
						size="m"
						pin="circle-circle"
						href={REPO}
						aria-label="GitHub"
					>
						<Button.Icon>
							<Icon data={LogoGithub} size={16} />
						</Button.Icon>
					</Button>
				</div>
			</div>
		</div>
	);
}
