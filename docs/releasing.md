# Releasing

A release is a **manifest** describing one or more binaries and an **Ed25519
signature** over that manifest's exact bytes, carried together in one envelope.
The binaries are not signed individually: the manifest names each one's
SHA-256, so one signature covers everything and there is never a question about
which bytes it covered. The manifest is decoded only *after* the signature
verifies, so a forged document is never parsed at all.

## The signing key

```sh
psgnss-release keygen                 # writes ~/.ssh/psgnss-release.key, 0600
```

It prints the public half to paste into `internal/release/pubkey.go`.

**The verifying key is compiled into the binary.** A station accepts only
releases signed by the key its own binary already carries, which has a
consequence worth stating before it bites: **lose the signing key and no
installed station can be updated in place again** — each one has to be handed a
new binary by hand, carrying the new public key, before automatic updates work
again.

Keep the key off the stations. Back it up. A build with no key in
`pubkey.go` verifies nothing rather than everything, which is the correct
behaviour for a development build.

## Making one

```sh
make release                                      # refuses a dirty tree
make sign-release BASE=https://host/path/v0.7.0 NOTES="..."
```

`make release` builds twice and confirms the hashes match, and refuses to build
from a dirty working tree — the version string is baked in at link time, and a
binary built mid-work reports the last commit plus `-dirty` while production
claims a release it is not running.

`make sign-release` then signs what was built and folds in the dependency
inventory, writing `dist/release.json`. Publish that and the binary under
`BASE`, and point each station's `update.manifest_url` at the `release.json`
URL.

```sh
psgnss-release verify dist/release.json           # against this build's key
```

## The dependency inventory

```sh
psgnssd --dependencies          # readable
psgnssd --dependencies-json     # for the release tool
```

**No version number in it is typed by hand.** Go module versions come from the
build information the linker embeds, which survives `-trimpath` and
`-buildvcs=false`. Each vendored browser work's version is read from the
filename it is served under, because the page loads `uplot-1.6.32.min.js` by
name and the name is the only place the version exists. `convbin` is asked.

A list of versions in a file is wrong the day after it is written, and wrong
silently. A test fails the build if a declared work is no longer vendored, or
if anything is reported without a licence.

Carrying the inventory in the manifest is what lets `--update-check` on a
station say what a release would change about the outside works it carries,
before it takes it.

## What a station does with it

In order, and every check that can happen before anything is replaced does:

1. the manifest's signature;
2. the download's size and SHA-256 against the signed manifest;
3. the new binary runs at all (`--version`);
4. **the new binary accepts this station's configuration**
   (`--check --config`) — validation here is strict and gets stricter, so a
   release that added a required key would otherwise start, refuse the
   configuration and leave the station down;
5. only now: the running binary is kept as `psgnssd.previous` and the new one
   moved into place;
6. restart, then ask the daemon's own `/api/status` whether it came back;
7. if it does not answer, the previous binary goes back and the service is
   restarted again.

The station ends up running the new release or the old one. Never neither.
