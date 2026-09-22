# Security

## Reporting

Open an issue for anything that is not itself exploitable. For something that
is, contact the maintainer privately first.

## What this software assumes

**It runs as root.** It owns a serial device and reads a root-only key, and the
systemd unit constrains it heavily in exchange: `ProtectSystem=strict`,
`NoNewPrivileges`, `ProtectHome`, `PrivateTmp`, `MemoryDenyWriteExecute`,
restricted address families and namespaces, and an explicit `DeviceAllow` for
the serial port. Changing those is a security decision, not a tidy-up.

**NTRIP passwords are recoverable by design.** They are encrypted under a master
key, not hashed, because an operator has to read them back to configure field
equipment. The consequence is stated plainly wherever it matters: the master key
at `/etc/psgnss/secrets/master.key` is equivalent to every stored password at
once. Keep it 0600, and back it up somewhere other than the database.
Administrator logins are hashed with argon2id and are not recoverable.

**A settings backup contains live credentials.** It is sealed with a passphrase
the operator chooses and which is never stored. Whoever has the file and the
passphrase has every NTRIP password in it.

## Publishing the web UI

The dashboard, data history and file download pages can be served to anyone;
everything else requires an administrator session. `internal/web/access.go`
holds the rules and the rate limits, and
`TestPublicSurfaceIsExactlyTheReadOnlyPages` is the list of what an anonymous
caller may reach. **Adding a route to that list is publishing that route** —
treat it as such.

Forwarded-address headers are trusted only from a loopback or private peer, so
a tunnel client running on the same machine is believed and nothing else is.
The anonymous payload is deliberately smaller than the administrator one; if a
public page needs a field that is stripped, add it consciously.

Putting an authenticating proxy in front of the administrator paths is a
reasonable second factor and is not provided here.

## Updates

Releases are signed with Ed25519 and verified against a key compiled into the
binary; see [docs/releasing.md](docs/releasing.md). A build with no key
verifies nothing rather than everything. Nothing updates on a timer.
