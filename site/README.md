# site

The landing page and step-by-step guide at https://floatdrop.github.io/di/,
in English, Russian, Chinese and Japanese, prerendered to static HTML.

It is a React project built with [Gravity UI](https://gravity-ui.com), and it
ships no React: `vite build` produces a server bundle, `scripts/prerender.ts`
runs it once per locale and writes the HTML out. The only JavaScript on the
page is `src/inline-script.ts`, inlined into the head. It settles the theme
class before the first paint, and drives the five things on the page that
move: the theme button, the copy button beside the install line, the active
item in the table of contents, the star count in the GitHub button, and
dismissing the language menu. Everything else, the menu's own open and close
included, is markup and CSS.

```sh
npm ci
npm run check          # tsc --noEmit
npm run dev            # http://localhost:5173/ and /ru/
npm run build          # build/ is the site
npm run preview        # serves build/ the way Pages does

BASE_PATH=/di npm run build    # what CI deploys; preview picks the base up
```

Without `BASE_PATH` the build's URLs are relative, so `build/index.html` can
be opened straight off the filesystem. With one they are absolute, which is
what Pages needs. `npm run preview` reads the base out of the built HTML
rather than the environment, and redirects anything outside it.

`.github/workflows/pages.yml` builds on pull requests that touch `site/` or
`examples/guide/` and deploys on pushes to `main`.

## How it fits together

- `src/content/*.tsx` is one file per locale, each satisfying `Content` in
  `src/content/types.ts`, so a missing translation is a compile error rather
  than a gap on the page. Prose is JSX, not strings in a catalog, because most
  sentences carry inline code and links. Adding a locale is `LOCALES` plus
  `localePath` in `types.ts`, the file, and two lines in `content/index.ts`;
  the compiler names the two, and the URLs, the `hreflang` links and the
  language menu all follow from `LOCALES` on their own.
  **`tsc` checks that a translation is complete, not that it is current.**
  Change an English paragraph and the other three still compile and deploy.
- `src/code.ts` reads the files of `../examples/guide` as raw text and
  highlights them with Shiki at build time, so the page cannot drift from code
  the Go CI compiles and tests. The `Explain` tree and the `Modules` report are
  `../examples/guide/testdata/`, pinned by golden tests. None of it is
  translated: it is Go source and output the Go tests own.
- `src/App.tsx` builds a figure per code block and hands the set to each
  section, which places the ones it shows. Everything visible is a Gravity UI
  component or a shape in `src/styles/main.css` that uikit has none for -- a
  prose column, a code figure, a sticky sidebar -- built from uikit's tokens.
- Both themes come out of Shiki as custom properties on every token, and the
  `g-root_theme_*` class on the html element picks one. Switching the theme
  re-highlights nothing and loads nothing.
- The table of contents is uikit's `Toc`. It takes its active item from a
  `value` prop, which a page with no React cannot change as you scroll, so the
  script sets the class `Toc` would have set and the component draws the rail.
  The language menu is a `details` with a uikit `Menu` in it, for the same
  reason: it is the chrome of `DropdownMenu` without the runtime.
- `src/selectors.ts` holds the ids and class names three places have to agree
  on: the markup, the inlined script, and the stylesheet that draws the
  states. Nothing re-renders, so both states of a toggle are always in the
  markup and CSS shows the one that applies.
- The GitHub button renders as the bare mark and the script appends the star
  count to it, which is what turns it into a pill: uikit sizes a button as
  icon-only through `:has(.g-button__icon:only-child)`, so a placeholder
  element in the markup would widen the circle before there was anything to
  put in it. With no JavaScript, no network, or no answer from GitHub, the
  button stays the circle it was rendered as. It is also the page's only
  third-party request, and it is made after paint.
- Type sizes are uikit's `--g-text-*` tokens, never numbers. uikit's scale is
  built for application density and a page of prose reads a step or two up it,
  so the choice of step is the design; the sizes are not ours to invent. Prose
  is `body-3` (17px), inline code `code-inline-3` (16px, the one step that
  keeps it on the line), the sidebar and captions `body-1` (13px), code blocks
  `code-2` (14px).

## Adding a step

Add its id to `StepId`, then add the step to `steps` in **every** locale;
`tsc` will not let you forget one. If it shows a new file, add the id
to `FigureId`, the import to `src/code.ts`, and the figure to `buildFigures`.

## Eight things worth knowing

`src/inline-script.ts` is inlined by calling `Function.prototype.toString` on
it, so it is cut out of its module and must close over nothing: everything it
uses is an argument or a global. Give it an import and the page gets a script
that references a name the browser has never heard of, which the build will
not notice. If you change it, load a built page and click the buttons.

`vite.config.ts` sets `ssr.noExternal` for Gravity UI. Its components import
their own CSS, and those imports are what build the stylesheet: with uikit
left external there is a page with no component styles on it, and the build
still succeeds. `build.ssrEmitAssets` is what writes that stylesheet out.

Every symptom of a missing stylesheet on this page looks like a design
mistake rather than a missing file: the `details` shows its disclosure marker,
`Toc` becomes a plain list of blue links, the buttons fall back to the
platform's own borders. Nothing 404s in the page you are looking at. So when
something looks wrong in one browser and not another, check that the CSS
loaded before believing anything else -- that is what the relative URLs above
are for.

Chinese and Japanese get their own font stacks, keyed off the html element's
`lang`, and they have to be separate ones: Han unification gives the two
languages the same codepoints with different correct glyph shapes, so one
shared CJK stack sets one of them in the other's hand. They also get their own
measure and leading, because `ch` is the width of a zero and a CJK glyph is a
full em, which makes the Latin `68ch` about 34 characters a line.

uikit's `.g-root` carries `font-size: 13px`, and this page puts that class on
the html element, because the inlined script has to settle the theme before
the body exists. That makes `1rem` 13px for the whole document unless it is
put back, which `src/styles/main.css` does in one rule near the top -- without
it every page metric written in rem is three-quarters of what it reads as, and
nothing about the page looks broken enough to point at the cause.

Dev gets its CSS from `src/dev-styles.ts`, not from a link to `main.css`.
Every uikit component imports its own stylesheet, a build collects those out
of the server bundle, and the dev server -- which ships no client bundle --
has nothing to collect them with. That is why every uikit component is
imported through `src/uikit.ts`: it is the one list both paths read, so dev
and the build style the same set. Import a component around it and it is
styled in the build and bare in dev. `scripts/prerender.ts` checks the built
stylesheet for `.g-button` and friends, which catches the same loss on the
other side.

Gravity UI's own `styles/fonts.css` pulls Inter from Google Fonts. Nothing
third-party is on the path to rendering the page, so `src/styles/main.css`
overrides the font stack instead; there is a comment there saying how to get
Inter back.

`site/build` is not only the site. `pages.yml` writes `coverage.html` and
`coverage.json` into it after `npm run build` and before the artifact is
uploaded, so the coverage report is served by the same deploy as the pages
that link to it. Two consequences: a local `npm run build` gives a tree
without them, and the report is the one thing on this site no local build
reproduces; and the workflow's path filter has `**/*.go` in it, so a change to
Go code deploys the site. Narrow that filter back to `site/` and the badge in
the root README goes on reading a figure from whenever the site last changed,
with nothing failing to say so.
