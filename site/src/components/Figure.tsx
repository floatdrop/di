import { Card } from '../uikit.ts';
import type { ReactNode } from 'react';

/** A captioned block: a file of the guide, or output a Go test pins. */
export function Figure({ caption, children }: { caption: string; children: ReactNode }) {
	return (
		<figure className="di-figure">
			<Card view="outlined" className="di-figure__card">
				<figcaption className="di-figure__caption">{caption}</figcaption>
				{children}
			</Card>
		</figure>
	);
}

/** Markup Shiki produced at build time. */
export function Highlighted({ html }: { html: string }) {
	return <div className="di-figure__body" dangerouslySetInnerHTML={{ __html: html }} />;
}

/** Text shown as it is: an Explain tree, a Modules report. */
export function Plain({ children }: { children: ReactNode }) {
	return (
		<pre className="di-figure__body">
			<code>{children}</code>
		</pre>
	);
}
