// The guide's code is the application under examples/guide, read at build
// time and highlighted once here, so the page is plain HTML and the code on
// it is code the Go toolchain compiles and tests.
import { codeToHtml } from 'shiki';

import main from '../../../examples/guide/cmd/api/main.go?raw';
import config from '../../../examples/guide/internal/config/config.go?raw';
import storage from '../../../examples/guide/internal/storage/storage.go?raw';
import storageTest from '../../../examples/guide/internal/storage/storage_test.go?raw';
import cache from '../../../examples/guide/internal/cache/cache.go?raw';
import cacheTest from '../../../examples/guide/internal/cache/cache_test.go?raw';
import mail from '../../../examples/guide/internal/mail/mail.go?raw';
import api from '../../../examples/guide/internal/api/api.go?raw';
import explain from '../../../examples/guide/testdata/explain.txt?raw';
import modules from '../../../examples/guide/testdata/modules.txt?raw';

const run = `git clone https://github.com/floatdrop/di && cd di
go run ./examples/guide/cmd/api &
curl -H 'X-User: ada' localhost:8080/users/42`;

const highlight = (code: string, lang = 'go') =>
	codeToHtml(code.trimEnd(), {
		lang,
		themes: { light: 'github-light', dark: 'github-dark' },
		defaultColor: false
	});

export const load = async () => ({
	code: {
		main: await highlight(main),
		config: await highlight(config),
		storage: await highlight(storage),
		storageTest: await highlight(storageTest),
		cache: await highlight(cache),
		cacheTest: await highlight(cacheTest),
		mail: await highlight(mail),
		api: await highlight(api),
		run: await highlight(run, 'sh')
	},
	explain: explain.trimEnd(),
	modules: modules.trimEnd()
});
