import { C } from '../components/prose.tsx';
import { PKG_DOC, REPO } from '../config.ts';
import { Link } from '../uikit.ts';
import type { Content } from './types.ts';

export const zh: Content = {
	locale: 'zh',
	htmlLang: 'zh-Hans',
	nativeName: '中文',

	meta: {
		title: 'di — Go 的依赖注入容器',
		description:
			'面向 Go 1.27+ 的依赖注入容器，基于泛型方法。一个应用从头到尾：它如何组织，以及它长什么样。'
	},

	hero: {
		title: '基于泛型方法的 Go 依赖注入。',
		lead: (
			<>
				注册服务，用<C>s.Get[T]()</C>解析它们，剩下的交给容器：创建、启动、停止，并检查依赖图。无需代码生成，没有第三方依赖。
			</>
		),
		install: 'go get github.com/floatdrop/di'
	},

	labels: {
		steps: '步骤',
		language: '语言',
		copy: '复制安装命令',
		toLight: '切换到浅色主题',
		toDark: '切换到深色主题'
	},

	intro: (
		<>
			这个页面本身就是一个应用，从上往下读：一个小型 HTTP
			服务，带一个数据库、挡在它前面的一层缓存、一个在后台运行的邮件服务，以及一个按请求创建的处理器。每个代码块都是仓库中
			<Link href={`${REPO}/tree/main/examples/guide`}>
				<C>examples/guide</C>
			</Link>
			里的一个文件。Go 工具链在每次改动时都会编译并测试它们，所以你看到的就是实际运行的代码。
		</>
	),

	tree: [
		{ path: 'cmd/api/main.go', comment: '组合模块，检查依赖图，运行' },
		{ path: 'internal/config/', comment: '配置，注册为一个值' },
		{ path: 'internal/storage/', comment: '数据库，以及构建在它之上的存储' },
		{ path: 'internal/cache/', comment: '包在存储外面的缓存' },
		{ path: 'internal/mail/', comment: '一个后台 worker' },
		{ path: 'internal/api/', comment: 'HTTP 服务器及其处理器' }
	],

	steps: [
		{
			id: 'shape',
			title: '应用的结构',
			body: (f) => (
				<>
					<p>
						每个包拥有自己的服务，并只暴露一个函数<C>Module</C>来注册它们。除此之外，包没有任何特别之处：构造函数就是普通函数，类型就是普通类型，唯一知道整张图的文件是<C>main</C>。
					</p>
					{f.tree}
				</>
			)
		},
		{
			id: 'constructors',
			title: '构造函数与模块',
			body: (f) => (
				<>
					<p>
						<C>newDB</C>和<C>newPGStore</C>通过参数接收它们需要的东西，并返回它们创建的东西；<C>newDB</C>
						可能失败。两者都不导入容器。这个应用里没有任何地方需要通用形式<C>Provide</C>，
						它接收一个对作用域的闭包；不过两种形式可以自由混用。
					</p>
					<p>
						<C>Module</C>通过<C>Wire</C>把它们交给容器。类型参数就是服务对外提供时使用的键：连接用
						<C>*db</C>，存储用<C>Store</C>接口，因为<C>*pgStore</C>
						可以赋值给它。传给<C>Wire</C>
						的构造函数的参数就是它的依赖，容器正是这样在创建任何东西之前就知道整张图。钩子按它们接收的值来定型，在应用启动和停止时运行。
					</p>
					<p>
						这里的私有性就是 Go 的私有性。键是类型，所以只有这个包能命名的<C>*db</C>，
						也就只有这个包能解析。包导出的是它的契约<C>Store</C>和<C>User</C>，以及它的
						<C>Module</C>；连接有一个由容器管理的生命周期，除此之外与谁都无关。
					</p>
					{f.storage}
				</>
			)
		},
		{
			id: 'config',
			title: '配置就是一个值',
			body: (f) => (
				<>
					<p>
						已经拿在手上的值用<C>Value</C>注册。它和其他键没有区别：<C>NewDB</C>
						通过参数拿到它，而测试用<C>Override()</C>把它替换掉，下游的每个服务都会跟着改变。
					</p>
					{f.config}
				</>
			)
		},
		{
			id: 'main',
			title: 'main 中的组合',
			body: (f) => (
				<>
					<p>
						<C>Use</C>
						按顺序应用各个模块，并把每次注册归属到做出它的那个模块，两个模块冲突时错误里指出的就是这个。同一个键的第二次注册如果没有
						<C>Override()</C>会被拒绝，并同时指出两处，所以一个模块无法悄悄改掉另一个模块的接线。
					</p>
					<p>
						<C>Validate</C>
						遍历已声明的图而不创建任何东西，同时被告知请求作用域里将会有什么。没有人提供的依赖、循环依赖、被单例捕获的请求级服务，都会在这里、在启动时失败，而不是在第一个请求上。然后
						<C>Run</C>启动 eager 服务，等待信号，并在超时时间内按相反顺序停止一切。
					</p>
					{f.main}
				</>
			)
		},
		{
			id: 'workers',
			title: '后台 worker',
			body: (f) => (
				<>
					<p>
						用<C>Go</C>注册的 worker 与它的服务同寿：服务启动时它在自己的 goroutine 里启动，由<C>Stop</C>
						取消，并且在它依赖的任何东西被拆除之前会等它结束。从它返回错误会停止整个应用。<C>Eager</C>
						表示邮件服务在<C>Start</C>返回时就已经存在，而不是等到第一次使用才创建。
					</p>
					{f.mail}
				</>
			)
		},
		{
			id: 'http',
			title: 'HTTP 与请求作用域',
			body: (f) => (
				<>
					<p>
						<C>dihttp.Middleware</C>为每个请求打开一个子作用域，其中持有<C>*http.Request</C>。
						在应用作用域中声明为<C>Scoped</C>
						的服务，会在每个请求作用域中创建一次，来源既可以是单例也可以是请求级的值，并随作用域一起停止。处理器通过
						<C>di.FromContext</C>拿到自己的作用域，或者通过<C>dihttp.Handle</C>，
						它会替处理器类型的方法完成这件事。
					</p>
					<p>
						一个处理器类型覆盖一个资源，每条路由一个方法，所以它的依赖只声明一次。
						<C>dihttp.Handle((*Users).Show)</C>
						从请求作用域解析出该类型并调用方法；方法表达式同时指明了两者，所以不需要类型参数。<C>Users</C>是
						<C>Scoped</C>，因为它需要调用方；<C>Health</C>不需要请求里的任何东西，是一个普通单例，而
						<C>Handle</C>对两种生命周期都适用。
					</p>
					<p>
						middleware 自己也需要作用域，才能为每个请求打开子作用域，所以<C>dihttp.Module</C>
						把它注册为一个服务，服务器像接收其他东西一样通过参数拿到它。<C>OnDrain</C>
						在任何东西被停止之前运行，所以在<C>http.Server.Shutdown</C>
						等待期间，正在处理中的请求仍然保有它们的作用域。
					</p>
					{f.api}
				</>
			)
		},
		{
			id: 'wrap',
			title: '包装而不是替换',
			body: (f) => (
				<>
					<p>
						<C>Wrap</C>
						在已经提供某个键的东西之上做组合。包装器第一个参数接收那个值，其余依赖排在它后面。存储保留自己的注册和钩子，先被创建，并在包装器之后停止，而包装器会把自己不改变的东西透传下去。唯一需要注意的是模块顺序：缓存的模块排在
						storage
						的后面。在子作用域中注册的包装器只作用于该作用域及其后代。和 storage 一样，这个包也只导出它的
						<C>Module</C>：横切关注点应当组合在导出的契约之上，而不是包的内部实现之上。
					</p>
					{f.cache}
				</>
			)
		},
		{
			id: 'testing',
			title: '用覆盖来测试',
			body: (f) => (
				<>
					<p>
						<C>di.Test</C>
						把这些模块接入一个全新的作用域，并在测试结束时停止它。只要覆盖配置，就能把存储指向另一个数据库；换成假实现也完全一样，写作
						<C>s.Value(&amp;fake).Override()</C>。
						这个标记是必须的：没有它的第二次注册会被拒绝，所以测试不会意外地在生产接线上通过。
					</p>
					{f.storageTest}
					<p>
						包装器也用同样的模块来测试，不替换任何东西：两次查询，一次命中。这是一个内部测试，因为只有
						cache 包能命名它自己的缓存。
					</p>
					{f.cacheTest}
				</>
			)
		},
		{
			id: 'graph',
			title: '看见依赖图',
			body: (f) => (
				<>
					<p>
						在创建任何东西之前，<C>Explain</C>就能画出传给<C>Wire</C>
						的构造函数所声明的内容：虚线的边、存储之上的包装器，以及是谁声明了你问到的那个服务。创建之后，它用实线画出实际发生的事情，后面跟着谁需要它。
						<C>Graph</C>把整个应用输出为 Graphviz DOT。这是启动时的
						<C>app.Explain[storage.Store]()</C>，由仓库里的一个测试固定下来。
					</p>
					{f.explain}
					<p>
						<C>Modules</C>
						是同样的信息，只是按模块而不是按服务来读：每个模块提供什么，需要什么以及由谁提供，包装了什么，以及它的哪些构造函数是闭包。它由注册信息推导而来，所以没有需要另行维护的清单。这是创建任何东西之前的整个应用，同样由测试固定下来。
					</p>
					{f.modules}
				</>
			)
		},
		{
			id: 'run',
			title: '运行',
			body: (f) => (
				<>
					<p>
						Ctrl-C
						会让服务器处理完正在进行的请求，取消邮件服务，关闭数据库，正是这个顺序，并报告每一个失败的钩子。
					</p>
					{f.run}
					<p>
						<Link href={`${REPO}#readme`}>README</Link>
						讲了其余部分：分组、观察者，以及容器强制执行的规则。
						<Link href={`${REPO}/blob/main/docs/DESIGN.md`}>它是如何工作的</Link>
						则从另一个方向讲起，从<C>Get</C>到一个值：生命周期、阶段、循环和关闭，并配有图示。
					</p>
				</>
			)
		}
	],

	footer: (
		<>
			<Link href={REPO}>github.com/floatdrop/di</Link> · <Link href={PKG_DOC}>pkg.go.dev</Link> ·
			MIT 许可证 · 这个页面由仓库的<C>site/</C>目录构建，页面上展示的代码来自
			<C>examples/guide/</C>。
		</>
	)
};
