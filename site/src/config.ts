export const REPO = 'https://github.com/floatdrop/di';
export const PKG_DOC = 'https://pkg.go.dev/github.com/floatdrop/di';

/**
 * GitHub Pages serves a project site under /<repo>; the workflow sets
 * BASE_PATH=/di. Every in-site URL goes through `url`.
 */
export const BASE = (process.env['BASE_PATH'] ?? '').replace(/\/$/, '');

/**
 * An in-site URL, from a page `depth` directories below the build root.
 *
 * With a base path the URLs are absolute, which is what Pages needs. Without
 * one they are relative to the page carrying them, so `build/` can be opened
 * straight off the filesystem or served from any prefix. Absolute paths with
 * no base look right and are wrong in both of those cases: the stylesheet
 * 404s, and a page with no CSS does not look like a broken link, it looks
 * like a broken design.
 */
export const url = (path: string, depth = 0) => {
	const clean = path.replace(/^\//, '');
	if (BASE) {
		return `${BASE}/${clean}`;
	}
	const up = '../'.repeat(depth);
	return clean === '' ? up || './' : up + clean;
};
