import { Link } from '../uikit.ts';

import { C } from '../components/prose.tsx';
import { PKG_DOC, REPO } from '../config.ts';
import type { Content } from './types.ts';

export const en: Content = {
	locale: 'en',
	htmlLang: 'en',
	nativeName: 'English',

	meta: {
		title: 'di — dependency injection for Go',
		description:
			'A dependency-injection container for Go 1.27+, built on generic methods. One application, top to bottom: how it is structured and what it looks like.'
	},

	hero: {
		title: 'Dependency injection for Go, built on generic methods.',
		lead: (
			<>
				Register services, resolve them with <C>s.Get[T]()</C>, and let the container build,
				start, stop and check the graph. No code generation, no dependencies.
			</>
		),
		install: 'go get github.com/floatdrop/di'
	},

	labels: {
		steps: 'Steps',
		language: 'Language',
		copy: 'Copy the install command',
		toLight: 'Switch to the light theme',
		toDark: 'Switch to the dark theme'
	},

	intro: (
		<>
			This page is one application, read top to bottom: a small HTTP service with a database, a
			cache in front of it, a mailer running in the background, and a handler built per request.
			Every code block is a file from{' '}
			<Link href={`${REPO}/tree/main/examples/guide`}>
				<C>examples/guide</C>
			</Link>{' '}
			in the repository. The Go toolchain compiles and tests them on every change, so what you
			see is what runs.
		</>
	),

	tree: [
		{ path: 'cmd/api/main.go', comment: 'composes the modules, checks the graph, runs' },
		{ path: 'internal/config/', comment: 'settings, registered as a value' },
		{ path: 'internal/storage/', comment: 'the database, and the store built on it' },
		{ path: 'internal/cache/', comment: 'a cache wrapped around the store' },
		{ path: 'internal/mail/', comment: 'a background worker' },
		{ path: 'internal/api/', comment: 'the HTTP server and its handlers' }
	],

	steps: [
		{
			id: 'shape',
			title: 'The shape of an application',
			body: (f) => (
				<>
					<p>
						Each package owns its services and exposes one function, <C>Module</C>, that
						registers them. Nothing else about a package is special: constructors are plain
						functions, types are plain types, and the only file that knows the whole graph is{' '}
						<C>main</C>.
					</p>
					{f.tree}
				</>
			)
		},
		{
			id: 'constructors',
			title: 'Constructors and a module',
			body: (f) => (
				<>
					<p>
						<C>newDB</C> and <C>newPGStore</C> take what they need as parameters and return
						what they make; <C>newDB</C> can fail. Neither imports the container. Nothing in
						this application needs the general form, <C>Provide</C>, which takes a closure
						over the scope; the two mix freely when something does.
					</p>
					<p>
						<C>Module</C> hands them over with <C>Wire</C>. The type argument is the key the
						service is served under: <C>*db</C> for the connection, and the <C>Store</C>{' '}
						interface for the store, since a <C>*pgStore</C> is assignable to it. The
						parameters of a wired constructor are its dependencies, which is how the container
						knows the graph before anything is built. Hooks are typed on the value they receive
						and run when the application starts and stops.
					</p>
					<p>
						Privacy is Go's. Keys are types, so <C>*db</C>, which only this package can name,
						is a service only this package can resolve. The package exports its contract,{' '}
						<C>Store</C> and <C>User</C>, and its <C>Module</C>; the connection has a lifecycle
						the container runs and is otherwise nobody else's business.
					</p>
					{f.storage}
				</>
			)
		},
		{
			id: 'config',
			title: 'Configuration is a value',
			body: (f) => (
				<>
					<p>
						A value you already have is registered with <C>Value</C>. It is a key like any
						other: <C>NewDB</C> receives it as a parameter, and a test replaces it with{' '}
						<C>Override()</C> and every service downstream follows.
					</p>
					{f.config}
				</>
			)
		},
		{
			id: 'main',
			title: 'Composition in main',
			body: (f) => (
				<>
					<p>
						<C>Use</C> applies the modules in order and attributes each registration to the
						module that made it, which is what an error names when two modules collide. A
						second registration of a key without <C>Override()</C> is rejected, naming both, so
						one module cannot rewire another unnoticed.
					</p>
					<p>
						<C>Validate</C> walks the declared graph without building anything, told what a
						request scope will hold. A dependency nothing provides, a cycle, or a
						request-scoped service captured by a singleton fails here, at startup, rather than
						on the first request. Then <C>Run</C> starts the eager services, waits for a
						signal, and stops everything in reverse order within the timeout.
					</p>
					{f.main}
				</>
			)
		},
		{
			id: 'workers',
			title: 'Background workers',
			body: (f) => (
				<>
					<p>
						A <C>Worker</C> runs for as long as its service does: started in its own goroutine
						when the service starts, cancelled by <C>Stop</C>, and waited for before anything
						it depends on is torn down. Returning an error from it stops the application.{' '}
						<C>Eager</C> says the mailer exists by the time <C>Start</C> returns rather than on
						first use.
					</p>
					{f.mail}
				</>
			)
		},
		{
			id: 'http',
			title: 'HTTP and request scopes',
			body: (f) => (
				<>
					<p>
						A <C>dihttp.Middleware</C> opens a child scope for each request, holding the{' '}
						<C>*http.Request</C>. Services declared <C>Scoped</C> in the application scope are
						built once per request scope, from singletons and request-scoped values alike, and
						stopped with it. A handler reaches its scope through <C>di.FromContext</C>, or
						through <C>dihttp.Handle</C>, which does that for a handler type's method.
					</p>
					<p>
						A handler type covers one resource, with a method per route, so its dependencies
						are declared once. <C>dihttp.Handle((*Users).Show)</C> resolves the type from the
						request scope and calls the method; a method expression names both, so no type
						argument is needed. <C>Users</C> is <C>Scoped</C> because it needs the caller;{' '}
						<C>Health</C> needs nothing from the request and is an ordinary singleton, and{' '}
						<C>Handle</C> follows either lifetime.
					</p>
					<p>
						The middleware needs the scope itself, to open a child per request, so{' '}
						<C>dihttp.Module</C> registers it as a service and the server takes it as a
						parameter like anything else. <C>OnDrain</C> runs before anything is stopped, so
						requests in flight keep their scopes while <C>http.Server.Shutdown</C> waits for
						them.
					</p>
					{f.api}
				</>
			)
		},
		{
			id: 'wrap',
			title: 'Wrapping without replacing',
			body: (f) => (
				<>
					<p>
						<C>Wrap</C> composes over whatever serves a key. The wrapper takes that value first
						and its other dependencies after it. The store keeps its registration and its
						hooks, is built first, and is stopped after the wrapper, and the wrapper forwards
						what it does not change. The one thing to get right is module order: the cache's
						module comes after storage's. A wrapper registered in a child scope applies to that
						scope and its descendants only. Like storage, this package exports only its{' '}
						<C>Module</C>: a cross-cutting concern composes over an exported contract, never
						over a package's internals.
					</p>
					{f.cache}
				</>
			)
		},
		{
			id: 'testing',
			title: 'Testing by overriding',
			body: (f) => (
				<>
					<p>
						<C>di.Test</C> wires the modules into a fresh scope and stops it when the test
						ends. Overriding the configuration is enough to point the store at another
						database; a fake would be <C>s.Value(&amp;fake).Override()</C> just the same. The
						marker is required: a second registration without it is rejected, so a test cannot
						pass against production wiring by accident.
					</p>
					{f.storageTest}
					<p>
						The wrapper is tested through the same modules, with nothing faked: two lookups,
						one hit. It is an internal test, because only the cache package can name its own
						cache.
					</p>
					{f.cacheTest}
				</>
			)
		},
		{
			id: 'graph',
			title: 'Seeing the graph',
			body: (f) => (
				<>
					<p>
						Before anything is built, <C>Explain</C> draws what the wired constructors
						declared: dashed edges, the wrapper over the store, and which service declares the
						one you asked about. After a build it draws what actually happened, solid, followed
						by what needed it. <C>Graph</C> renders the whole application as Graphviz DOT. This
						is <C>app.Explain[storage.Store]()</C> at startup, pinned by a test in the
						repository.
					</p>
					{f.explain}
					<p>
						<C>Modules</C> is the same information read by module rather than by service: what
						each provides, what it needs and who serves it, what it wraps, and which of its
						constructors are closures. It is derived from the registrations, so there is no
						manifest to keep in step. This is the whole application, before anything is built,
						pinned by a test as well.
					</p>
					{f.modules}
				</>
			)
		},
		{
			id: 'run',
			title: 'Run it',
			body: (f) => (
				<>
					<p>
						Ctrl-C drains the server, cancels the mailer, closes the database, in that order,
						and reports any hook that failed.
					</p>
					{f.run}
					<p>
						The <Link href={`${REPO}#readme`}>README</Link> covers the rest: groups, observers,
						and the rules the container enforces.{' '}
						<Link href={`${REPO}/blob/main/docs/DESIGN.md`}>How it works</Link> goes the other
						way, from <C>Get</C> to a value: lifetimes, phases, cycles and shutdown, with
						diagrams.
					</p>
				</>
			)
		}
	],

	footer: (
		<>
			<Link href={REPO}>github.com/floatdrop/di</Link> ·{' '}
			<Link href={PKG_DOC}>pkg.go.dev</Link> · MIT licensed · This page is built from the
			repository's <C>site/</C> directory and the code it shows from <C>examples/guide/</C>.
		</>
	)
};
