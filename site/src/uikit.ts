/**
 * Every Gravity UI component the page uses, re-exported from one module.
 *
 * **Import uikit components from here, never from `@gravity-ui/uikit`
 * directly.** Each component imports its own CSS, and that is where the
 * page's stylesheet comes from -- but the two ways it gets there differ:
 * a production build collects it out of the server bundle's module graph,
 * while the dev server has no client bundle to collect anything from and
 * loads `src/dev-styles.ts` instead. Routing every component through one
 * module is what keeps those two sets identical. A component imported
 * around it is styled in the build and unstyled in dev.
 */
export { Button, Card, Icon, Link, Menu, Text, ThemeProvider, Toc } from '@gravity-ui/uikit';
