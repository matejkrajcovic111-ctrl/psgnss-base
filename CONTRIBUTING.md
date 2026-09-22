# Contributing

## Build and check

```sh
make test vet
gofmt -l . && staticcheck ./...
node --check internal/web/assets/app.js
make verify              # two builds, identical hashes
```

All of those are expected to be silent.

## What this codebase values

**Comments record why, not what.** Several of them are measurements that cost a
day to obtain — why `convbin -f` must be 4, why a mount unit's filename has to
be the escaped path, why the station ID is 0. Those are the most valuable lines
in the repository. Do not tidy them away.

**Verify against the real thing.** Check `str2str -h` rather than remembering
what a flag does; run `systemd-escape` rather than re-reading the manual;
measure with `/proc/<pid>/io` rather than concluding that a running process is
a working one. Where a claim in a commit message is a number, it should be a
number someone measured.

**Say what was proven and what was reasoned through.** Both are legitimate. A
commit that says "this path has only ever been reasoned about, never executed"
is more useful than one that implies otherwise.

**The configuration refuses rather than guesses.** An unknown key is fatal. A
setting that would produce quietly wrong data is rejected at startup. Keep it
that way: it is what lets the updater ask a new release whether it would accept
a station's configuration before replacing anything.

**No new dependency without a reason.** The daemon links four modules and
vendors its browser libraries. A static binary with no runtime dependencies is
a feature of a device that sits on a pole.

## The web UI

There is no build step and no Node in the product. `app.js` is edited directly.
Colours come from CSS custom properties — never hardcode one, add a token.

`tools/uicheck` renders the working tree against a live station in a headless
browser so a change can be looked at rather than imagined. Two bugs shipped
because nobody did that; both were found the first time someone did.

## Tests

A test should fail for a reason someone can act on. Prefer one that reproduces
the real failure — a real `gdalinfo`, a real `systemd-analyze verify`, a real
pty — over one that asserts the arguments a function built.
