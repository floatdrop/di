import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit()],
	// The guide imports the Go sources of examples/guide as raw text, and
	// they live above this project.
	server: { fs: { allow: ['..'] } }
});
