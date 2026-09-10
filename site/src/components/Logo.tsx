// The mascot is docs/assets/logo.svg, the file the root README shows, imported
// at build time the way src/code.ts imports the guide's code, so the site
// cannot drift from it. Its paths go into the page once, as a group inside a
// zero-sized svg, and every place that shows the logo is a `use` of that group
// in an svg of its own. A viewBox belongs to the instance, not the sprite,
// which is what lets the hero frame the whole gopher and the topbar only the
// face: at 24px the cables are a smear and the face still reads.
import raw from '../../../docs/assets/logo.svg?raw';

// The file's outer element and its accessible name stay in the file: here
// each instance is an svg of its own, and decorative, since the text beside
// it carries the name.
const inner = raw
	.replace(/^[\s\S]*?<svg[^>]*>/, '')
	.replace(/<\/svg>\s*$/, '')
	.replace(/<title[^>]*>[\s\S]*?<\/title>/, '')
	.replace(/<desc[^>]*>[\s\S]*?<\/desc>/, '');
if (inner === raw || !inner.includes('<g ')) {
	throw new Error('docs/assets/logo.svg is not the svg element with a group in it that Logo.tsx unwraps');
}

const SPRITE_ID = 'di-logo';

/**
 * The frames an instance can show: the whole figure, or the head down to the
 * connectors, which is also what public/favicon.svg is cut to.
 */
const frames = {
	full: '0 0 400 400',
	face: '96 44 208 208'
} as const;

/** The paths, once. Rendered first in the body; the instances point at it. */
export function LogoSprite() {
	return (
		<svg className="di-logo-sprite" aria-hidden="true" focusable="false">
			<defs>
				<g id={SPRITE_ID} dangerouslySetInnerHTML={{ __html: inner }} />
			</defs>
		</svg>
	);
}

interface LogoProps {
	frame: keyof typeof frames;
	className: string;
}

/** One drawing of the logo, sized by its class. */
export function Logo({ frame, className }: LogoProps) {
	return (
		<svg className={className} viewBox={frames[frame]} aria-hidden="true" focusable="false">
			<use href={`#${SPRITE_ID}`} />
		</svg>
	);
}
