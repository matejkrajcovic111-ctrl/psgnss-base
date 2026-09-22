#!/usr/bin/env bash
#
# PSGNSS_base installer.
#
#   curl -fsSL https://raw.githubusercontent.com/matejkrajcovic111-ctrl/psgnss-base/main/install.sh | sudo bash
#
# or, from a checkout:
#
#   sudo ./install.sh
#
# It gets the source, makes sure there is a Go toolchain, builds the daemon,
# hands over to the first-run interview and then starts the service. Nothing
# here is clever: every step says what it is about to do, and the one that asks
# the questions is `psgnssd --setup`, which refuses to touch a station that is
# already configured.
#
# Environment:
#   PSGNSS_REPO     git URL to install from (default below)
#   PSGNSS_REF      branch or tag (default main)
#   PSGNSS_ROOT     write the install under this directory instead of /, and
#                   skip systemd entirely. For trying it out.
#   PSGNSS_FORCE=1  replace an existing installation
#   PSGNSS_YES=1    do not pause for confirmation

set -euo pipefail

REPO="${PSGNSS_REPO:-https://github.com/matejkrajcovic111-ctrl/psgnss-base}"
REF="${PSGNSS_REF:-main}"
ROOT="${PSGNSS_ROOT:-}"
FORCE="${PSGNSS_FORCE:-}"
ASSUME_YES="${PSGNSS_YES:-}"

# The toolchain is pinned and its checksum with it. A build that silently used
# whatever Go happened to be current would stop being the reproducible build
# this project checks with `make verify`.
GO_VERSION="1.24.7"
GO_SHA256_amd64="da18191ddb7db8a9339816f3e2b54bdded8047cdc2a5d67059478f8d1595c43f"
GO_SHA256_arm64="fd2bccce882e29369f56c86487663bb78ba7ea9e02188a5b0269303a0c3d33ab"

WORKDIR=""
cleanup() { [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ] && rm -rf "$WORKDIR"; }
trap cleanup EXIT

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf '\n\033[1;31mInstall stopped:\033[0m %s\n\n' "$*" >&2; exit 1; }

# open_tty puts the terminal on file descriptor 3, or fails.
#
# `[ -r /dev/tty ]` is not the test: the device node exists and is readable
# whenever /dev is mounted, and opening it still fails with "no such device or
# address" when the process has no controlling terminal -- which is exactly the
# case this matters for. The only reliable check is to open it.
open_tty() { exec 3</dev/tty 2>/dev/null; }
close_tty() { exec 3<&- 2>/dev/null || true; }

# ---------------------------------------------------------------- preflight

[ "$(uname -s)" = "Linux" ] || die "PSGNSS runs on Linux. This is $(uname -s)."

case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) die "unsupported architecture $(uname -m). PSGNSS builds for arm64 and amd64." ;;
esac

if [ -z "$ROOT" ] && [ "$(id -u)" != "0" ]; then
  die "run this with sudo. It writes to /etc, /opt and /var, and installs a systemd unit."
fi

if [ -z "$ROOT" ] && ! command -v systemctl >/dev/null 2>&1; then
  die "systemd not found. PSGNSS ships a systemd service; on another init system you
  would need to write your own unit. deploy/psgnss.service is the reference."
fi

for tool in curl tar; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and is not installed."
done

CONFIG="${ROOT}/etc/psgnss/psgnss.toml"
if [ -e "$CONFIG" ] && [ -z "$FORCE" ]; then
  die "this machine already has a station at $CONFIG.

  To change settings, use the Settings page in the web UI: it edits one thing at
  a time and keeps the database and the master key.

  To rebuild from scratch, back up /etc/psgnss/secrets/master.key and
  /var/lib/psgnss/psgnss.db first, then run again with PSGNSS_FORCE=1."
fi

cat <<BANNER

  PSGNSS_base — GNSS base station

  This will build the daemon from source and set up a station on this machine:
  ${ARCH}, $( [ -n "$ROOT" ] && echo "under $ROOT (a trial install, no services touched)" || echo "installing to /opt/psgnss, /etc/psgnss, /var/lib/psgnss" )

BANNER

if [ -z "$ASSUME_YES" ]; then
  # A curl | bash pipeline owns stdin -- it is the script being read -- so the
  # terminal has to be reopened for anything that asks a question. This is the
  # first of two places that matters; the interview is the other.
  if open_tty; then
    printf '  Continue? [y/N] '
    read -r reply <&3
    close_tty
  else
    die "there is no terminal to ask on. Re-run with PSGNSS_YES=1, or download the
  script and run it directly so it can ask its questions."
  fi
  case "$reply" in y|Y|yes|YES) ;; *) echo "  Nothing was changed."; exit 0 ;; esac
fi

# ------------------------------------------------------------- the source

WORKDIR="$(mktemp -d)"

if [ -f "$(dirname "$0")/go.mod" ] && grep -q "psgnss-base" "$(dirname "$0")/go.mod" 2>/dev/null; then
  SRC="$(cd "$(dirname "$0")" && pwd)"
  say "Building from this checkout"
  info "$SRC"
else
  say "Downloading the source"
  SRC="$WORKDIR/src"
  mkdir -p "$SRC"
  tarball="${REPO%.git}/archive/refs/heads/${REF}.tar.gz"
  info "$tarball"
  curl -fsSL "$tarball" | tar -xz -C "$SRC" --strip-components=1 ||
    die "could not download $tarball. Check PSGNSS_REPO and PSGNSS_REF."
  [ -f "$SRC/go.mod" ] || die "that archive does not look like the PSGNSS source."
fi

# --------------------------------------------------------------- toolchain

need_go() {
  command -v go >/dev/null 2>&1 || return 0
  # 1.24 or newer. Sort -V puts the required version first if go is older.
  have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  [ -z "$have" ] && return 0
  lowest="$(printf '%s\n%s\n' "$have" "1.24" | sort -V | head -1)"
  [ "$lowest" = "1.24" ] && return 1 || return 0
}

if need_go; then
  say "Installing the Go toolchain ${GO_VERSION} (temporary, into $WORKDIR)"
  info "No Go ${GO_VERSION}+ was found. This is used to build the daemon and is"
  info "then thrown away with the rest of the working directory."
  case "$ARCH" in
    amd64) want="$GO_SHA256_amd64" ;;
    arm64) want="$GO_SHA256_arm64" ;;
  esac
  archive="$WORKDIR/go.tar.gz"
  curl -fsSL -o "$archive" "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" ||
    die "could not download the Go toolchain."
  got="$(sha256sum "$archive" | awk '{print $1}')"
  [ "$got" = "$want" ] || die "the Go toolchain download does not match its published checksum.
  expected $want
  got      $got
  Nothing was installed."
  info "checksum ok"
  tar -xzf "$archive" -C "$WORKDIR"
  export PATH="$WORKDIR/go/bin:$PATH"
  export GOROOT="$WORKDIR/go"
  export GOPATH="$WORKDIR/gopath"
  export GOCACHE="$WORKDIR/gocache"
else
  say "Using the Go toolchain already installed"
  info "$(go version)"
fi

# ------------------------------------------------------------------- build

say "Building psgnssd"
info "a few minutes on a Pi, and it needs the network for the Go modules"
BIN="$WORKDIR/psgnssd"
(
  cd "$SRC"
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= \
      -X github.com/psgnss/psgnss-base/internal/version.Version=$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo "$REF") \
      -X github.com/psgnss/psgnss-base/internal/version.Commit=$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    -o "$BIN" ./cmd/psgnssd
) || die "the build failed. The output above says why."
chmod 0755 "$BIN"
info "$("$BIN" --version)"

# --------------------------------------------------------------- interview

say "Setting up the station"

setup_args=(--setup)
[ -n "$ROOT" ]  && setup_args+=(--setup-root "$ROOT")
[ -n "$FORCE" ] && setup_args+=(--setup-force)

# The interview asks questions, so it wants the terminal rather than this
# script's stdin, which under `curl | bash` is the script itself. With no
# terminal at all -- a scripted install, or a test -- stdin is what is left and
# the interview reads its answers from there.
if open_tty; then
  "$BIN" "${setup_args[@]}" <&3 || die "setup did not finish. Nothing has been started."
  close_tty
else
  "$BIN" "${setup_args[@]}" || die "setup did not finish. Nothing has been started."
fi

[ -e "$CONFIG" ] || { echo; info "Setup was cancelled, so nothing was started."; exit 0; }

# ----------------------------------------------------------------- service

if [ -n "$ROOT" ]; then
  say "Trial install finished"
  info "Everything was written under $ROOT and no service was touched."
  info "Look at $ROOT/etc/psgnss/psgnss.toml and $ROOT/etc/systemd/system/."
  exit 0
fi

say "Starting the service"
systemd-tmpfiles --create /etc/tmpfiles.d/psgnss.conf
systemctl daemon-reload

# The archive mount unit, if the interview configured one. Its name is the
# escaped mount path and systemd accepts no other, so it is derived with
# systemd's own escaper rather than by searching for the file.
mount_point="$(sed -n 's/^[[:space:]]*mount_point[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)"
if [ -n "$mount_point" ]; then
  unit="$(systemd-escape --path --suffix=mount "$mount_point")"
  if [ -f "/etc/systemd/system/$unit" ]; then
    info "archive share: $unit"
    systemctl enable --now "$unit" ||
      info "the share did not mount; the station records to its local spool until it does"
  fi
fi

systemctl enable --now psgnss.service
info "waiting for it to serve"

web_port="$(sed -n 's/^[[:space:]]*listen[[:space:]]*=[[:space:]]*"[^"]*:\([0-9]*\)".*/\1/p' "$CONFIG" | tail -1)"
web_port="${web_port:-8090}"
ok=""
for _ in $(seq 1 45); do
  sleep 1
  if curl -fsS -m 3 "http://127.0.0.1:${web_port}/api/status" >/dev/null 2>&1; then ok=1; break; fi
done

if [ -z "$ok" ]; then
  echo
  die "the service did not answer on port ${web_port} within 45 seconds.
  Nothing was removed. Look at what it says:
      systemctl status psgnss.service
      journalctl -u psgnss.service -n 50"
fi

host="$(hostname -I 2>/dev/null | awk '{print $1}')"
host="${host:-127.0.0.1}"

cat <<DONE

$(printf '\033[1;32m==> PSGNSS is running.\033[0m')

    Web UI     http://${host}:${web_port}
    Status     systemctl status psgnss.service
    Logs       journalctl -u psgnss.service -f

    Back up /etc/psgnss/secrets/master.key somewhere other than this machine.
    It is what makes stored NTRIP passwords readable, and a backup holding both
    it and the database is a plaintext password list. Keep them apart.

    No rover accounts exist yet. Add them in the web UI, or:
        psgnssd --config /etc/psgnss/psgnss.toml --user-add name:password:5

DONE

if ! [ -x /opt/psgnss/bin/convbin ]; then
  cat <<'CONVBIN'
    One thing is not installed: convbin, from RTKLIB, which PSGNSS runs to turn
    raw logs into RINEX. Everything else works without it; the RINEX downloader
    will not. Build it from https://github.com/rtklibexplorer/RTKLIB and put it
    at /opt/psgnss/bin/convbin.

CONVBIN
fi
