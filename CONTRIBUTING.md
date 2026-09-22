# Contributing

## Build and check

```sh
make test vet
gofmt -l . && staticcheck ./...
node --check internal/web/assets/app.js
make verify              # two builds, identical hashes
```

All of those should be silent.

## What this codebase cares about

**Comments explain why, not what.** A lot of them are measurements that took a
day to get: why `convbin -f` has to be 4, why a mount unit's filename must be
the escaped path, why the station ID is 0. Those are some of the most valuable
lines here, so please don't tidy them away.

**Check against the real thing.** Read `str2str -h` rather than remembering
what a flag does. Run `systemd-escape` rather than re-reading the manual. Use
`/proc/<pid>/io` rather than concluding that a running process is a working
one. If a commit message contains a number, it should be a number someone
measured.

**Be clear about what was proven and what was reasoned through.** Both are
fine. A commit that says "this path has only ever been reasoned about, never
executed" is more useful than one that quietly implies otherwise.

**The config refuses rather than guesses.** An unknown key is fatal, and a
setting that would produce quietly wrong data gets rejected at startup. Keeping
that property is what lets the updater ask a new release whether it would
accept a station's configuration before replacing anything.

**Don't add a dependency without a reason.** The daemon links four modules and
vendors its browser libraries. A static binary with no runtime dependencies is
a feature when the thing is bolted to a pole.

## The web UI

There's no build step and no Node in the product; `app.js` is edited directly.
Colours come from CSS custom properties, so add a token rather than hardcoding
a value.

`tools/uicheck` renders the working tree against a live station in a headless
browser, so you can look at a change instead of imagining it. Two bugs shipped
because nobody did that, and both were found the first time somebody did.

## Tests

A test should fail for a reason you can act on. Where you can, reproduce the
real failure — a real `gdalinfo`, a real `systemd-analyze verify`, a real pty —
rather than asserting the arguments a function built.
