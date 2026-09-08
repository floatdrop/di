// The dev server's stylesheet, and only the dev server's.
//
// A production build gets its CSS from the server bundle's module graph:
// every Gravity UI component imports its own, and `build.ssrEmitAssets`
// writes the collection out as one file. The dev server renders through the
// same modules but ships no client bundle, so nothing collects those imports
// and a plain link to main.css would leave the page with uikit's tokens and
// none of its components -- which does not look like missing CSS, it looks
// like a badly built page.
//
// So dev pulls them in as module side effects instead, in the order the
// bundle puts them: components first, main.css last, since a few of its rules
// sit at the same specificity as uikit's and win on order alone.
import './uikit.ts';
import './styles/main.css';
