#!/bin/sh
# Install the sessionhub binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/abdallah/session-hub/main/install.sh | sh
#
# Environment:
#   SESSIONHUB_VERSION      release tag to install, e.g. v0.1.1 (default: latest)
#   SESSIONHUB_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   GITHUB_TOKEN            only needed while the repository is private
#
# Needs: sh, curl or wget, tar, and sha256sum or shasum.
set -eu

REPO="abdallah/session-hub"
INSTALL_DIR="${SESSIONHUB_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${SESSIONHUB_VERSION:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# --- platform -----------------------------------------------------------------
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $(uname -s) (linux and darwin only)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m) (amd64 and arm64 only)" ;;
esac

# --- tools --------------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { # fetch URL OUT [ACCEPT]
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: ${3:-application/json}" -o "$2" "$1"
    else
      curl -fsSL -H "Accept: ${3:-application/json}" -o "$2" "$1"
    fi
  }
elif command -v wget >/dev/null 2>&1; then
  fetch() {
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      wget -q --header="Authorization: Bearer $GITHUB_TOKEN" --header="Accept: ${3:-application/json}" -O "$2" "$1"
    else
      wget -q --header="Accept: ${3:-application/json}" -O "$2" "$1"
    fi
  }
else
  die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum"
fi
command -v tar >/dev/null 2>&1 || die "need tar"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

# --- release metadata ---------------------------------------------------------
api="https://api.github.com/repos/$REPO/releases"
if [ -n "$VERSION" ]; then
  fetch "$api/tags/$VERSION" "$tmp/release.json" || die "release $VERSION not found"
else
  fetch "$api/latest" "$tmp/release.json" || die "could not read the latest release"
fi
tag="$(sed -n 's/^  "tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)"
[ -n "$tag" ] || die "no tag_name in the release metadata"
ver="${tag#v}"
archive="session-hub_${ver}_${os}_${arch}.tar.gz"

# asset_url NAME: the API URL of a release asset (works for private repos too)
asset_url() {
  tr -d '\n' <"$tmp/release.json" | tr '{' '\n' | grep "\"name\": *\"$1\"" |
    sed -n 's/.*"url": *"\([^"]*\/releases\/assets\/[0-9]*\)".*/\1/p' | head -n 1
}

# --- already installed? -------------------------------------------------------
if [ -x "$INSTALL_DIR/sessionhub" ]; then
  current="$("$INSTALL_DIR/sessionhub" version 2>/dev/null | awk '{print $2}')" || current=""
  if [ "${current#v}" = "$ver" ]; then
    say "sessionhub $tag is already installed in $INSTALL_DIR"
    exit 0
  fi
fi

# --- download and verify ------------------------------------------------------
for f in "$archive" checksums.txt; do
  url="$(asset_url "$f")"
  [ -n "$url" ] || die "release $tag has no asset $f"
  fetch "$url" "$tmp/$f" application/octet-stream || die "download failed: $f"
done
want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$want" = "$got" ] || die "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" sessionhub || die "sessionhub not found in $archive"

# --- install (rename, so a running process keeps its old copy) ----------------
mkdir -p "$INSTALL_DIR"
cp "$tmp/sessionhub" "$INSTALL_DIR/sessionhub.new"
chmod 0755 "$INSTALL_DIR/sessionhub.new"
mv -f "$INSTALL_DIR/sessionhub.new" "$INSTALL_DIR/sessionhub"

say "installed sessionhub $tag to $INSTALL_DIR/sessionhub"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "note: $INSTALL_DIR is not on your PATH" ;;
esac
say ""
say "Next: enroll this machine against your sessionhub server:"
say "  sessionhub join <server-ssh-target>"
say "See docs/join.md."
