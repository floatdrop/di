// A development server for a site that has no client bundle: Vite in
// middleware mode serves the modules, and every request renders the page the
// way the build does, so what you look at is the output, not an approximation.
import { createServer as createHttpServer } from 'node:http';
import { createServer as createViteServer } from 'vite';

import type { Page, Styles } from '../src/entry-server.tsx';

// Not a link to main.css: that would leave out every Gravity UI component's
// own stylesheet, which in a build comes from the server bundle's module
// graph. See src/dev-styles.ts.
const DEV_STYLES: Styles = { module: '/src/dev-styles.ts' };

const vite = await createViteServer({ server: { middlewareMode: true }, appType: 'custom' });

const server = createHttpServer((req, res) => {
	vite.middlewares(req, res, async () => {
		const path = new URL(req.url ?? '/', 'http://localhost').pathname;
		try {
			const { render } = (await vite.ssrLoadModule('/src/entry-server.tsx')) as {
				render: (styles: Styles) => Promise<Page[]>;
			};
			const pages = await render(DEV_STYLES);
			const wanted = path.replace(/^\//, '').replace(/index\.html$/, '');
			const page = pages.find((p) => p.file.replace(/index\.html$/, '') === wanted);

			if (!page) {
				res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' });
				res.end(`no page at ${path}\n`);
				return;
			}

			// Injects the Vite client, so an edit reloads the page.
			const html = await vite.transformIndexHtml(path, page.html);
			res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
			res.end(html);
		} catch (error) {
			const failure = error instanceof Error ? error : new Error(String(error));
			vite.ssrFixStacktrace(failure);
			res.writeHead(500, { 'content-type': 'text/plain; charset=utf-8' });
			res.end(failure.stack ?? failure.message);
		}
	});
});

const port = Number(process.env['PORT'] ?? 5173);
server.listen(port, () => {
	console.log(`  http://localhost:${port}/    English`);
	console.log(`  http://localhost:${port}/ru/ Russian`);
});
