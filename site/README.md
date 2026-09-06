# site

The landing page and step-by-step guide at https://floatdrop.github.io/di/,
a SvelteKit project prerendered to static HTML with no client-side
JavaScript.

The guide's code blocks are the files of `../examples/guide`, imported as raw
text at build time and highlighted once with Shiki, so the page cannot drift
from code the Go CI compiles and tests. The `Explain` tree it shows is
`../examples/guide/testdata/explain.txt`, pinned by a golden test.

```sh
npm ci
npm run check          # svelte-check
npm run dev            # http://localhost:5173
BASE_PATH=/di npm run build   # what the Pages workflow does; build/ is the site
```

`.github/workflows/pages.yml` builds on pull requests that touch `site/` or
`examples/guide/` and deploys on pushes to `main`.
