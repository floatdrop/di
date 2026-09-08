// Serves build/ the way GitHub Pages does, including the base path, so the
// prerendered output can be checked before it is deployed.
import { createReadStream } from 'node:fs';
import { readFile, stat } from 'node:fs/promises';
import { createServer } from 'node:http';
import { dirname, extname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const outDir = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'build');

// The base comes out of the build rather than the environment. Reading
// BASE_PATH here means a `BASE_PATH=/di` build previewed without it serves a
// page whose stylesheet 404s, which does not look like a mistake in the
// command -- it looks like the page has no design.
const built = await readFile(join(outDir, 'index.html'), 'utf8');
const base = /href="(\/[^"]*)\/assets\//.exec(built)?.[1] ?? '';

const types: Record<string, string> = {
	'.html': 'text/html; charset=utf-8',
	'.css': 'text/css; charset=utf-8',
	'.js': 'text/javascript; charset=utf-8',
	'.svg': 'image/svg+xml',
	'.txt': 'text/plain; charset=utf-8'
};

const server = createServer(async (req, res) => {
	let path = new URL(req.url ?? '/', 'http://localhost').pathname;
	if (base && !path.startsWith(base)) {
		// Serving this build outside its base would find index.html and then
		// none of the assets it asks for, which reads as a broken page rather
		// than a wrong URL. Send the visitor where the build actually lives.
		res.writeHead(302, { location: base + '/' }).end();
		return;
	}
	path = path.slice(base.length);
	if (path.endsWith('/')) {
		path += 'index.html';
	}

	const file = join(outDir, path);
	if (!file.startsWith(outDir)) {
		res.writeHead(403).end();
		return;
	}

	try {
		const info = await stat(file);
		if (!info.isFile()) {
			throw new Error('not a file');
		}
		res.writeHead(200, {
			'content-type': types[extname(file)] ?? 'application/octet-stream',
			'content-length': info.size
		});
		createReadStream(file).pipe(res);
	} catch {
		res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' });
		res.end(`not found: ${path}\n`);
	}
});

const port = Number(process.env['PORT'] ?? 4173);
server.listen(port, () => console.log(`  http://localhost:${port}${base}/`));
