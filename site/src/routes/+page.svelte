<script lang="ts">
	let { data } = $props();

	const repo = 'https://github.com/floatdrop/di';
	const steps = [
		{ id: 'shape', title: 'The shape of an application' },
		{ id: 'constructors', title: 'Constructors and a module' },
		{ id: 'config', title: 'Configuration is a value' },
		{ id: 'main', title: 'Composition in main' },
		{ id: 'workers', title: 'Background workers' },
		{ id: 'http', title: 'HTTP and request scopes' },
		{ id: 'wrap', title: 'Wrapping without replacing' },
		{ id: 'testing', title: 'Testing by overriding' },
		{ id: 'graph', title: 'Seeing the graph' },
		{ id: 'run', title: 'Run it' }
	];
</script>

<svelte:head>
	<title>di — dependency injection for Go</title>
	<meta
		name="description"
		content="A dependency-injection container for Go 1.27+, built on generic methods. One application, top to bottom: how it is structured and what it looks like."
	/>
</svelte:head>

<header class="hero">
	<p class="kicker">github.com/floatdrop/di</p>
	<h1>Dependency injection for Go, built on generic methods.</h1>
	<p class="lead">
		Register services, resolve them with <code>s.Get[T]()</code>, and let the container build,
		start, stop and check the graph. No code generation, no dependencies.
	</p>
	<pre class="install"><code>go get github.com/floatdrop/di</code></pre>
	<p class="links">
		<a href={repo}>Source</a>
		<a href="https://pkg.go.dev/github.com/floatdrop/di">Reference</a>
		<a href="{repo}/releases">Releases</a>
	</p>
</header>

<div class="layout">
	<nav class="toc" aria-label="Steps">
		<ol>
			{#each steps as step, i (step.id)}
				<li><a href="#{step.id}">{i + 1}. {step.title}</a></li>
			{/each}
		</ol>
	</nav>

	<main>
		<p class="intro">
			This page is one application, read top to bottom: a small HTTP service with a database, a
			cache in front of it, a mailer running in the background, and a handler built per request.
			Every code block is a file from <a href="{repo}/tree/main/examples/guide"><code>examples/guide</code></a>
			in the repository. The Go toolchain compiles and tests them on every change, so what you
			see is what runs.
		</p>

		<section id="shape">
			<h2><span class="n">1</span>The shape of an application</h2>
			<p>
				Each package owns its services and exposes one function, <code>Module</code>, that
				registers them. Nothing else about a package is special: constructors are plain
				functions, types are plain types, and the only file that knows the whole graph is
				<code>main</code>.
			</p>
			<figure>
				<figcaption>examples/guide</figcaption>
				<pre class="tree">cmd/api/main.go       <span class="c">composes the modules, checks the graph, runs</span>
internal/config/      <span class="c">settings, registered as a value</span>
internal/storage/     <span class="c">the database, and the store built on it</span>
internal/cache/       <span class="c">a cache wrapped around the store</span>
internal/mail/        <span class="c">a background worker</span>
internal/api/         <span class="c">the HTTP server and its per-request handler</span></pre>
			</figure>
		</section>

		<section id="constructors">
			<h2><span class="n">2</span>Constructors and a module</h2>
			<p>
				<code>NewDB</code> and <code>NewPGStore</code> take what they need as parameters and
				return what they make; <code>NewDB</code> can fail. Neither imports the container.
			</p>
			<p>
				<code>Module</code> hands them over with <code>Wire</code>. The type argument is the
				key the service is served under: <code>*DB</code> for the connection, and the
				<code>Store</code> interface for the store, since a <code>*PGStore</code> is assignable
				to it. The parameters of a wired constructor are its dependencies, which is how the
				container knows the graph before anything is built. Hooks are typed on the value they
				receive and run when the application starts and stops.
			</p>
			<figure>
				<figcaption>internal/storage/storage.go</figcaption>
				{@html data.code.storage}
			</figure>
		</section>

		<section id="config">
			<h2><span class="n">3</span>Configuration is a value</h2>
			<p>
				A value you already have is registered with <code>Value</code>. It is a key like any
				other: <code>NewDB</code> receives it as a parameter, and a test replaces it with
				<code>Override()</code> and every service downstream follows.
			</p>
			<figure>
				<figcaption>internal/config/config.go</figcaption>
				{@html data.code.config}
			</figure>
		</section>

		<section id="main">
			<h2><span class="n">4</span>Composition in main</h2>
			<p>
				<code>Use</code> applies the modules in order and attributes each registration to the
				module that made it, which is what an error names when two modules collide. A second
				registration of a key without <code>Override()</code> is rejected, naming both, so one
				module cannot rewire another unnoticed.
			</p>
			<p>
				<code>Validate</code> walks the declared graph without building anything, told what a
				request scope will hold. A dependency nothing provides, a cycle, or a request-scoped
				service captured by a singleton fails here, at startup, rather than on the first
				request. Then
				<code>Run</code> starts the eager services, waits for a signal, and stops everything
				in reverse order within the timeout.
			</p>
			<figure>
				<figcaption>cmd/api/main.go</figcaption>
				{@html data.code.main}
			</figure>
		</section>

		<section id="workers">
			<h2><span class="n">5</span>Background workers</h2>
			<p>
				A <code>Worker</code> runs for as long as its service does: started in its own goroutine
				when the service starts, cancelled by <code>Stop</code>, and waited for before anything
				it depends on is torn down. Returning an error from it stops the application.
				<code>Eager</code> says the mailer exists by the time <code>Start</code> returns rather
				than on first use.
			</p>
			<figure>
				<figcaption>internal/mail/mail.go</figcaption>
				{@html data.code.mail}
			</figure>
		</section>

		<section id="http">
			<h2><span class="n">6</span>HTTP and request scopes</h2>
			<p>
				<code>dihttp.Middleware</code> opens a child scope for each request, holding the
				<code>*http.Request</code>. Services declared <code>Scoped</code> in the application
				scope are built once per request scope, from singletons and request-scoped values
				alike, and stopped with it. A handler reaches its scope through
				<code>di.FromContext</code>.
			</p>
			<p>
				The server's constructor needs the scope itself, to make that middleware, so it is a
				closure: <code>Provide</code> is the general form, <code>Wire</code> the declared one,
				and they mix freely. <code>OnDrain</code> runs before anything is stopped, so requests
				in flight keep their scopes while <code>http.Server.Shutdown</code> waits for them.
			</p>
			<figure>
				<figcaption>internal/api/api.go</figcaption>
				{@html data.code.api}
			</figure>
		</section>

		<section id="wrap">
			<h2><span class="n">7</span>Wrapping without replacing</h2>
			<p>
				<code>Wrap</code> composes over whatever serves a key. The wrapper takes that value
				first and its other dependencies after it. The store keeps its registration and its
				hooks, is built first, and is stopped after the wrapper. The one thing to get right is
				module order: the cache's module comes after storage's. A wrapper registered in a
				child scope applies to that scope and its descendants only.
			</p>
			<figure>
				<figcaption>internal/cache/cache.go</figcaption>
				{@html data.code.cache}
			</figure>
		</section>

		<section id="testing">
			<h2><span class="n">8</span>Testing by overriding</h2>
			<p>
				<code>di.Test</code> wires the modules into a fresh scope and stops it when the test
				ends. Overriding the configuration is enough to point the store at another database;
				a fake would be <code>s.Value(&amp;fake).Override()</code> just the same. The marker is
				required: a second registration without it is rejected, so a test cannot pass against
				production wiring by accident.
			</p>
			<figure>
				<figcaption>internal/storage/storage_test.go</figcaption>
				{@html data.code.storageTest}
			</figure>
			<p>
				The wrapper is tested through the same modules, with nothing faked: two lookups, one
				hit.
			</p>
			<figure>
				<figcaption>internal/cache/cache_test.go</figcaption>
				{@html data.code.cacheTest}
			</figure>
		</section>

		<section id="graph">
			<h2><span class="n">9</span>Seeing the graph</h2>
			<p>
				Before anything is built, <code>Explain</code> draws what the wired constructors
				declared: dashed edges, the wrapper over the store, and which service declares the one
				you asked about. After a build it draws what actually happened, solid, followed by what
				needed it. <code>Graph</code> renders the whole application as Graphviz DOT. This is
				<code>app.Explain[storage.Store]()</code> at startup, pinned by a test in the
				repository.
			</p>
			<figure>
				<figcaption>app.Explain[storage.Store]()</figcaption>
				<pre class="plain"><code>{data.explain}</code></pre>
			</figure>
		</section>

		<section id="run">
			<h2><span class="n">10</span>Run it</h2>
			<p>
				Ctrl-C drains the server, cancels the mailer, closes the database, in that order,
				and reports any hook that failed.
			</p>
			<figure>
				<figcaption>shell</figcaption>
				{@html data.code.run}
			</figure>
			<p>
				The <a href="{repo}#readme">README</a> covers the rest: groups, observers, the rules
				the container enforces, and the design notes on concurrency and shutdown.
			</p>
		</section>
	</main>
</div>

<footer>
	<p>
		<a href={repo}>github.com/floatdrop/di</a> · MIT licensed · This page is built from the
		repository's <code>site/</code> directory and the code it shows from
		<code>examples/guide/</code>.
	</p>
</footer>
