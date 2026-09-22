# Security

## Reporting

Open an issue for anything that isn't itself exploitable. For anything that is,
contact the maintainer privately first.

## What this software assumes

**It runs as root.** It owns a serial device and reads a root-only key. The
systemd unit constrains it heavily in return: `ProtectSystem=strict`,
`NoNewPrivileges`, `ProtectHome`, `PrivateTmp`, `MemoryDenyWriteExecute`,
restricted address families and namespaces, and an explicit `DeviceAllow` for
the serial port. Changing any of that is a security decision, not housekeeping.

**NTRIP passwords are recoverable by design.** They're encrypted under a master
key rather than hashed, because you have to read them back to configure field
equipment. The consequence is spelled out wherever it matters: the master key
at `/etc/psgnss/secrets/master.key` is equivalent to every stored password at
once. Keep it 0600 and back it up somewhere other than the database.
Administrator logins are hashed with argon2id and can't be recovered.

**A settings backup contains live credentials.** It's sealed with a passphrase
you choose, which is never stored. Anyone with the file and the passphrase has
every NTRIP password in it.

## Publishing the web UI

The dashboard, data history and file download pages can be served to anyone.
Everything else needs an administrator session. `internal/web/access.go` holds
the rules and the rate limits, and `TestPublicSurfaceIsExactlyTheReadOnlyPages`
is the list of what an anonymous caller may reach. Adding a route to that list
means publishing that route, so treat it accordingly.

Forwarded-address headers are trusted only from a loopback or private peer, so
a tunnel client on the same machine is believed and nothing else is. The
anonymous payload is deliberately smaller than the administrator one; if a
public page needs a field that's stripped out, add it deliberately.

Putting an authenticating proxy in front of the administrator paths is a
reasonable second factor. It isn't provided here.

## Updates

Releases are signed with Ed25519 and verified against a key compiled into the
binary; see [docs/releasing.md](docs/releasing.md). A build with no key
verifies nothing rather than everything. Nothing updates on a timer.
