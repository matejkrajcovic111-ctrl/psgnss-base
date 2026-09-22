# Operations

Running a station day to day.

## Is it working?

```sh
systemctl status psgnss.service
curl -s http://127.0.0.1:8090/api/status
printf 'GET / HTTP/1.0\r\n\r\n' | nc 127.0.0.1 2101     # the sourcetable
```

`/api/status` answering is the check that means something. "The unit is active"
is not: systemd calls a process that started and is about to exit active too.

The **Diagnostics** page runs the same checks with one button — receiver and
GNSS freshness, configuration, telemetry database, RINEX converter, every
mountpoint, every archive writer, the local spool and the network share. The
storage tests write and remove a small probe file, so they prove the path
rather than assuming it.

## Users

```sh
psgnssd --config /etc/psgnss/psgnss.toml --user-add rover:password:5
psgnssd --config /etc/psgnss/psgnss.toml --user-list
psgnssd --config /etc/psgnss/psgnss.toml --connections 20
```

Or the **Users** page: accounts, connection limits, expiry, IP and CIDR rules,
per-mountpoint access, and the full connection history for an account — when,
from where, for how long, how many bytes.

NTRIP passwords can be read back; that is the point of storing them encrypted
rather than hashed. Administrator passwords cannot.

## Backups

Two things matter, and they must not be kept together.

**The master key**, `/etc/psgnss/secrets/master.key`. Without it every stored
NTRIP password is unreadable. With it and a copy of the database, you have a
plaintext password list.

**The settings backup**, from Settings → Backup and restore. One encrypted
file holding the configuration, administrators, NTRIP accounts and their access
rules, the integrity monitor and the push-out targets — enough to rebuild the
station on new hardware. Not the telemetry, the connection log or the receiver
audit trail: those are records of what happened here, and restoring them onto
another machine would be fiction.

The file is sealed with a passphrase you choose, which is never stored. The
credentials inside travel as plaintext and are re-sealed under the receiving
station's own key, so the key itself never leaves the machine. Say it plainly:
**whoever has the file and the passphrase has every NTRIP password in it.**

## Updating

```sh
psgnssd --update-check
psgnssd --update
```

See the README for what the updater checks and in what order. If a release
fails to serve after installation, the previous binary is put back
automatically and left at `/opt/psgnss/bin/psgnssd.previous`; going back by
hand is:

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

Then check it is serving, not merely running, with the commands at the top.

## When something is wrong

**No corrections, but the stream looks healthy.** Check that the reference
station ID in the configuration matches what the receiver stamps. A mismatch
gives a rover satellites and never a fix. See
[hardware-notes.md](hardware-notes.md).

**The archive is empty but the share is mounted.** Check `ReadWritePaths` in
the service unit includes the mount point. `ProtectSystem=strict` makes every
write fail with "read-only file system" otherwise, and says so only in the log.

**The share never mounts.** The mount unit's filename must be the
systemd-escaped mount path — `/mnt/gnssraw` is `mnt-gnssraw.mount`. Any other
name and systemd never reads it. `systemd-escape --path --suffix=mount <path>`
gives the right name.

**RINEX has no BeiDou.** `rinex.frequencies` must be 4 on a ZED-X20P.

**RINEX has a `.obs` and no `.nav`.** The navigation archive needs to be
enabled, and needs both `RXM-SFRBX` and `RXM-RAWX`.

**The receiver stopped responding after a configuration change.** It did not:
an unconfirmed write reverts itself after `revert_timeout`. Wait, then look at
the receiver audit trail in the UI.

**The machine looks busy.** `psgnssd` uses about 0.35% of one core. Look for
something else, including a stray process left behind by an interrupted SSH
command.

## Logs

```sh
journalctl -u psgnss.service -f
```

The Diagnostics page also shows the last 1000 entries the daemon has logged
since it started, filterable by source, window, level and text. That buffer is
in memory and is lost on restart; the journal is not.
