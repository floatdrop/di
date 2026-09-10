import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

import { pages } from './scripts/serve.ts';

// There is one build: the server bundle. The page ships no JavaScript, so
// there is no client bundle to make -- but the CSS the rendered markup needs
// is exactly the CSS the server bundle imports (Gravity UI's components each
// import their own), so `ssrEmitAssets` collects it and scripts/prerender.ts
// copies the one file it produces next to the HTML.
export default defineConfig({
	plugins: [react(), pages()],
	// There is no index.html either: in dev the pages are rendered by
	// scripts/serve.ts on request, and a build writes them out.
	appType: 'custom',
	// The guide imports the Go sources of examples/guide as raw text, and the
	// logo docs/assets, and both live above this project.
	server: { fs: { allow: ['..'] } },
	// Gravity UI has to go through the bundler rather than be left to Node:
	// uikit's components import their own `.css`, which only Vite knows what
	// to do with, and it is those imports that build the stylesheet; and the
	// icons barrel that uikit itself reaches for uses extensionless paths Node
	// will not resolve. Everything else stays external, which keeps Shiki's
	// several hundred grammars out of a bundle that is run once and thrown
	// away.
	ssr: { noExternal: [/@gravity-ui\//] },
	build: {
		ssr: 'src/entry-server.tsx',
		outDir: '.ssr',
		emptyOutDir: true,
		copyPublicDir: false,
		ssrEmitAssets: true,
		// One stylesheet for the one page.
		cssCodeSplit: false,
		// The bundle is run once by the prerender script and thrown away; the
		// inlined theme script is read out of it with Function.prototype.toString,
		// so leaving it unminified keeps the page source legible.
		minify: false
	}
});
