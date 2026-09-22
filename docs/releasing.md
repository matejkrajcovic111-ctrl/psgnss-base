# Releasing

A release is a manifest describing one or more binaries, plus an Ed25519
signature over that manifest's exact bytes. The two travel together in one
envelope.

The binaries aren't signed individually. The manifest names each one's SHA-256,
so a single signature covers everything and there's no ambiguity about which
bytes it applied to. The manifest itself is base64 inside the envelope and only
gets decoded after the signature verifies, so a forged document never reaches
the parser.

## The signing key

```sh
psgnss-release keygen                 # writes ~/.ssh/psgnss-release.key, 0600
```

It prints the public half for you to paste into `internal/release/pubkey.go`.

The verifying key is compiled into the binary, which means a station only
accepts releases signed by the key it already carries. Worth understanding
before it bites: if you lose the signing key, no installed station can be
updated in place again. Each one has to be handed a new binary by hand,
carrying the new public key, before automatic updates work again.

Keep the key off the stations and back it up. A build with no key in
`pubkey.go` verifies nothing rather than everything, which is the right
behaviour for a development build.

## Making one

```sh
make release                                      # refuses a dirty tree
make sign-release BASE=https://host/path/v0.7.0 NOTES="..."
```

`make release` builds twice and confirms the hashes match. It refuses to build
from a dirty working tree, because the version string is baked in at link time:
a binary built mid-edit reports the last commit plus `-dirty`, and then
production claims a release it isn't running.

`make sign-release` signs what was built, folds in the dependency inventory and
writes `dist/release.json`. Publish that alongside the binary under `BASE`, and
point each station's `update.manifest_url` at the `release.json` URL.

```sh
psgnss-release verify dist/release.json           # against this build's key
```

## The dependency inventory

```sh
psgnssd --dependencies          # readable
psgnssd --dependencies-json     # for the release tool
```

None of the version numbers in it are typed by hand. Go module versions come
from the build information the linker embeds, which survives `-trimpath` and
`-buildvcs=false`. Each vendored browser library's version is read from the
filename it's served under, since the page loads `uplot-1.6.32.min.js` by name
and the name is the only place the version actually exists. `convbin` gets
asked.

A list of versions in a file is wrong the day after you write it, and wrong
silently. A test fails the build if a declared library is no longer vendored,
or if anything is reported without a licence.

Carrying the inventory in the manifest is what lets `--update-check` on a
station tell you what a release would change about its third-party libraries
before you take it.

## What a station does with it

In order, and everything that can be checked before anything is replaced is:

1. the manifest's signature
2. the download's size and SHA-256 against the signed manifest
3. whether the new binary runs at all (`--version`)
4. whether the new binary accepts this station's configuration
   (`--check --config`), since validation is strict and gets stricter, and a
   release that added a required key would otherwise start, refuse the config
   and leave the station down
5. only now: the running binary is kept as `psgnssd.previous` and the new one
   moved into place
6. restart, then ask the daemon's own `/api/status` whether it came back
7. if it doesn't answer, the previous binary goes back and the service
   restarts again

You end up on the new release or the old one, not stuck between them.
