// The second half of `npm run build`. `vite build` has produced the server
// bundle in .ssr/ together with the CSS the rendered markup needs; this runs
// the bundle once per locale and lays the result out as the site.
import { cp, mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import type { Page, Styles } from '../src/entry-server.tsx';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ssrDir = join(root, '.ssr');
const outDir = join(root, 'build');

const assets = await readdir(join(ssrDir, 'assets'));
const css = assets.filter((name) => name.endsWith('.css'));
if (css.length !== 1) {
	// cssCodeSplit is off and there is one entry, so anything else means the
	// build changed shape and the page would be missing styles.
	throw new Error(`expected one stylesheet in .ssr/assets, found ${css.length}: ${css.join(', ')}`);
}
const [cssName] = css as [string];
const cssFile = `assets/${cssName}`;

// The stylesheet is built out of the components' own CSS imports, and losing
// those -- an `ssr.noExternal` that stops matching, a component imported
// around src/uikit.ts -- still produces a stylesheet, still succeeds, and
// still deploys. It just deploys uikit's tokens with none of uikit on top.
const stylesheet = await readFile(join(ssrDir, cssFile), 'utf8');
for (const marker of ['.g-button', '.g-toc-item', '.g-menu', '.g-text']) {
	if (!stylesheet.includes(marker)) {
		throw new Error(`${cssFile} has no ${marker} rules: component CSS did not reach the bundle`);
	}
}

await rm(outDir, { recursive: true, force: true });
await cp(join(root, 'public'), outDir, { recursive: true });
await mkdir(join(outDir, 'assets'), { recursive: true });
await cp(join(ssrDir, cssFile), join(outDir, cssFile));

const bundle = pathToFileURL(join(ssrDir, 'entry-server.js')).href;
const { render } = (await import(bundle)) as { render: (styles: Styles) => Promise<Page[]> };

for (const page of await render({ link: cssFile })) {
	const file = join(outDir, page.file);
	await mkdir(dirname(file), { recursive: true });
	await writeFile(file, page.html);
	console.log(`  ${page.file}  ${(Buffer.byteLength(page.html) / 1024).toFixed(1)} kB`);
}
console.log(`  ${cssFile}`);
