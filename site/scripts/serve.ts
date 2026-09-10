// The dev server's half of the site, as a Vite plugin, so that `npm run dev`
// is Vite's own server -- its port picking, its banner, its keyboard
// shortcuts, its hot-module socket on the same port -- with one middleware
// added: a request for a page renders it through src/entry-server.tsx the
// way the build does, so what you look at is the output, not an
// approximation. scripts/prerender.ts is the build's half.
import type { Plugin, ViteDevServer } from 'vite';

import { LOCALES, localePath } from '../src/content/types.ts';
import type { Page, Styles } from '../src/entry-server.tsx';

// Not a link to main.css: that would leave out every Gravity UI component's
// own stylesheet, which in a build comes from the server bundle's module
// graph. See src/dev-styles.ts.
const DEV_STYLES: Styles = { module: '/src/dev-styles.ts' };

export function pages(): Plugin {
	return {
		name: 'di:pages',
		apply: 'serve',

		configureServer(server) {
			listPages(server);
			// Registered after Vite's own middlewares, so the client, the
			// modules under src/ and the files under public/ are Vite's to
			// serve, and only what is left reaches the renderer.
			return () => {
				server.middlewares.use(async (req, res, next) => {
					const path = new URL(req.url ?? '/', 'http://localhost').pathname;
					try {
						const { render } = (await server.ssrLoadModule('/src/entry-server.tsx')) as {
							render: (styles: Styles) => Promise<Page[]>;
						};
						const rendered = await render(DEV_STYLES);
						const wanted = path.replace(/^\//, '').replace(/index\.html$/, '');
						const page = rendered.find((p) => p.file.replace(/index\.html$/, '') === wanted);

						if (!page) {
							// Pages sends /ru to /ru/; do the same, and name the pages
							// there are for anything else.
							if (rendered.some((p) => p.file === `${wanted}/index.html`)) {
								res.writeHead(302, { location: `${path}/` }).end();
								return;
							}
							res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' });
							res.end(`no page at ${path}; the pages are ${pagePaths().join(' ')}\n`);
							return;
						}

						// Injects the Vite client, so an edit reloads the page.
						const html = await server.transformIndexHtml(path, page.html);
						res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
						res.end(html);
					} catch (error) {
						// Vite's error middleware prints it properly and answers 500.
						const failure = error instanceof Error ? error : new Error(String(error));
						server.ssrFixStacktrace(failure);
						next(failure);
					}
				});
			};
		},

		// The browser holds no module but the stylesheet, so Vite has nothing
		// to hot-swap when a component, a content file, a guide file or the
		// logo changes: they are all in the server's module graph only, and
		// the page is stale until it is reloaded. Reload it. A file the
		// browser does hold, main.css, is left to Vite, which swaps it in place.
		hotUpdate({ file, modules, server }) {
			if (this.environment.name !== 'ssr' || modules.length === 0) {
				return;
			}
			if (server.environments.client.moduleGraph.getModulesByFile(file)?.size) {
				return;
			}
			server.environments.client.hot.send({ type: 'full-reload', path: '*' });
		}
	};
}

const pagePaths = () => LOCALES.map((l) => `/${localePath[l]}`);

/** Adds the locales' pages to Vite's banner, under its Local line. */
function listPages(server: ViteDevServer) {
	const printUrls = server.printUrls.bind(server);
	server.printUrls = () => {
		printUrls();
		server.config.logger.info(`  ➜  Pages:   ${pagePaths().join('  ')}`);
	};
}
