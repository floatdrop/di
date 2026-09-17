<!-- CONTRIBUTING.md has the full text. This is the short form of it. -->

## What this changes

<!-- A sentence or two, and the issue if there is one. -->

<!-- Contributor License Agreement
     ==============================
     External contributors: Yandex asks for a one-time CLA declaration, and a
     bot will ask for it on this pull request with the exact sentence to use.
     Post that sentence as a COMMENT -- not here in the description, which the
     bot does not read. One declaration is enough, ever: if you have made it on
     an earlier pull request here, the bot already knows.

     It is deliberately not pre-filled anywhere: the declaration has to be one
     you actually made. CONTRIBUTING.md has the background. -->

## Checklist

- [ ] The gate below passes locally, as one chain, the way CI runs it.
- [ ] `CHANGELOG.md` has an entry under `## [Unreleased]` that says plainly
      whether an upgrade can break a caller -- or this is a docs- or site-only
      change, which adds none.
- [ ] A behaviour change has a test. A bug fix has a regression test that
      **fails against the commit before the fix**; say which commit, and tag the
      test the way its neighbours are tagged.
- [ ] Edited `examples/`? Ran `gofmt -w examples/` and re-embedded the README,
      or the sync check fails.
- [ ] Edited the library or its tests? The coverage badge and the guide site
      are built from them, so check `site/` still builds if you touched
      `examples/guide/`.

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... \
  && golangci-lint run ./... \
  && go run github.com/campoy/embedmd@v1.0.0 -d README.md
```

<!-- Generative suites are worth a run on anything touching resolution or
     teardown, since they reach what the hand-written tests do not:

       go test -race -run '^$' -fuzz FuzzMachineConcurrent -fuzztime 2m .
-->
