# Operations

Running a station day to day.

## Is it working?

```sh
systemctl status psgnss.service
curl -s http://127.0.0.1:8090/api/status
printf 'GET / HTTP/1.0\r\n\r\n' | nc 127.0.0.1 2101     # the sourcetable
```

`/api/status` answering is the check that means something. "The unit is active"
isn't, since systemd will happily call a process active while it's on its way
to exiting.

The Diagnostics page runs the same checks from one button: receiver and GNSS
freshness, configuration, telemetry database, RINEX converter, each mountpoint,
each archive writer, the local spool and the network share. The storage tests
write and remove a small probe file rather than assuming the path works.

## Users

```sh
psgnssd --config /etc/psgnss/psgnss.toml --user-add rover:password:5
psgnssd --config /etc/psgnss/psgnss.toml --user-list
psgnssd --config /etc/psgnss/psgnss.toml --connections 20
```

Or use the Users page, which handles accounts, connection limits, expiry, IP
and CIDR rules, per-mountpoint access, and the full history for an account:
when, from where, how long, how many bytes.

NTRIP passwords can be read back. That's the point of storing them encrypted
rather than hashed, since you need them to configure field equipment.
Administrator passwords can't.

## Backups

Two things matter, and you shouldn't keep them in the same place.

The master key at `/etc/psgnss/secrets/master.key`. Without it, every stored
NTRIP password is unreadable. With it and a copy of the database, you've got a
plaintext password list.

The settings backup, from Settings → Backup and restore. One encrypted file
with the configuration, administrators, NTRIP accounts and their access rules,
the integrity monitor and the push-out targets: enough to rebuild the station
on new hardware. It deliberately leaves out the telemetry, the connection log
and the receiver audit trail, since those are records of what happened *here*
and restoring them somewhere else would be fiction.

The file is sealed with a passphrase you choose, which is never stored. The
credentials inside travel as plaintext and get re-sealed under the receiving
station's own key, so the key itself never leaves the machine. Worth being
blunt about: anyone with the file and the passphrase has every NTRIP password
in it.

## Updating

```sh
psgnssd --update-check
psgnssd --update
```

The README describes what the updater checks and in what order. If a release
fails to serve after installation, the previous binary goes back automatically
and is left at `/opt/psgnss/bin/psgnssd.previous`. To go back by hand:

```sh
sudo install -m0755 /opt/psgnss/bin/psgnssd.previous /opt/psgnss/bin/psgnssd
sudo systemctl restart psgnss.service
```

## Deploying a build by hand

```sh
make arm64
scp dist/psgnssd-linux-arm64 user@station:/tmp/psgnssd.stage
ssh user@station '
  sudo systemctl stop psgnss.service &&
  sudo install -m0755 /tmp/psgnssd.stage /opt/psgnss/bin/psgnssd &&
  rm -f /tmp/psgnssd.stage &&
  sudo systemctl start psgnss.service'
```

Then check it's serving, not just running, using the commands at the top.

## When something's wrong

**No corrections, but the stream looks healthy.** Check that the reference
station ID in the config matches what the receiver stamps. A mismatch gives a
rover satellites and never a fix. See [hardware-notes.md](hardware-notes.md).

**The archive is empty but the share is mounted.** Check that `ReadWritePaths`
in the service unit includes the mount point. `ProtectSystem=strict` makes
every write fail with "read-only file system" otherwise, and only says so in
the log.

**The share never mounts.** The mount unit's filename has to be the
systemd-escaped mount path, so `/mnt/gnssraw` is `mnt-gnssraw.mount`. Anything
else and systemd never reads it. `systemd-escape --path --suffix=mount <path>`
gives you the right name.

**RINEX has no BeiDou.** `rinex.frequencies` needs to be 4 on a ZED-X20P.

**RINEX has a `.obs` and no `.nav`.** The navigation archive needs to be
enabled, with both `RXM-SFRBX` and `RXM-RAWX`.

**The receiver stopped responding after a config change.** It didn't. An
unconfirmed write reverts itself after `revert_timeout`. Wait, then look at the
receiver audit trail in the UI.

**The machine looks busy.** `psgnssd` uses about 0.35% of one core, so look
elsewhere, including at anything left behind by an interrupted SSH command.

## Logs

```sh
journalctl -u psgnss.service -f
```

The Diagnostics page also shows the last 1000 entries the daemon has logged
since it started, filterable by source, window, level and text. That buffer
lives in memory and disappears on restart; the journal doesn't.
