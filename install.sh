#!/bin/sh
# mole installer.
#
# curl -fsSL https://raw.githubusercontent.com/lajosdeme/mole/main/install.sh | sh
#
# Downloads the release archive for this platform, VERIFIES its SHA-256 against
# the checksums file published with the release, and installs two binaries.
#
# Piping a script from the internet into a shell is a thing to be uneasy about,
# and this one is written to be read first: no eval, no sudo unless it has to,
# every download checksummed, and a --dry-run that shows what it would do. If you
# would rather not, the README's "from source" path needs only Go.
#
# Environment:
#   MOLE_VERSION   tag to install (default: latest release)
#   MOLE_INSTALL   directory to install into (default: first writable of
#                  ~/.local/bin, /usr/local/bin)
#   MOLE_NO_SUDO   set to refuse privilege escalation outright

set -eu

REPO="lajosdeme/mole"
VERSION="${MOLE_VERSION:-}"
INSTALL_DIR="${MOLE_INSTALL:-}"
DRY_RUN=0

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	--version=*) VERSION="${arg#*=}" ;;
	--dir=*) INSTALL_DIR="${arg#*=}" ;;
	-h | --help)
		sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $arg" >&2
		exit 2
		;;
	esac
done

say() { printf '%s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required and not on PATH"
}

# --- platform -----------------------------------------------------------------

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported OS: $os. Build from source: go install github.com/$REPO/cmd/mole@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $arch. Build from source: go install github.com/$REPO/cmd/mole@latest" ;;
esac

need curl
need tar

# --- version ------------------------------------------------------------------

if [ -z "$VERSION" ]; then
	# Resolved from the redirect rather than the API, so this works without a
	# token and does not count against an unauthenticated rate limit that a
	# shared CI address will already have spent.
	resolved=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
		"https://github.com/$REPO/releases/latest" 2>/dev/null |
		sed 's#.*/tag/##')
	# Validated, not merely non-empty. When there is no release to redirect to —
	# or the repository is private, which is the case this was written against —
	# curl reports the URL it was given, sed finds no /tag/ to strip, and VERSION
	# became the whole URL. The installer then built
	# ".../download/https://github.com/.../mole_https://...tar.gz" and reported a
	# download failure, which sends the reader looking in the wrong place.
	case "$resolved" in
	v[0-9]*) VERSION="$resolved" ;;
	*)
		die "could not determine the latest version of $REPO.
Is the repository public, and has a release been published? Otherwise pass the
tag yourself: --version=vX.Y.Z" ;;
	esac
fi
# Tags are published with a leading v; archives are named without one.
NUM_VERSION="${VERSION#v}"

# --- destination --------------------------------------------------------------

writable() { [ -d "$1" ] && [ -w "$1" ]; }

if [ -z "$INSTALL_DIR" ]; then
	for candidate in "$HOME/.local/bin" /usr/local/bin; do
		if writable "$candidate"; then
			INSTALL_DIR="$candidate"
			break
		fi
	done
fi
# Nothing writable: prefer creating a user directory over asking for root. An
# installer that reaches for sudo when it does not need it teaches a bad habit.
if [ -z "$INSTALL_DIR" ]; then
	INSTALL_DIR="$HOME/.local/bin"
	mkdir -p "$INSTALL_DIR" 2>/dev/null || true
fi

SUDO=""
if ! writable "$INSTALL_DIR"; then
	if [ -n "${MOLE_NO_SUDO:-}" ]; then
		die "$INSTALL_DIR is not writable and MOLE_NO_SUDO is set"
	fi
	command -v sudo >/dev/null 2>&1 || die "$INSTALL_DIR is not writable and sudo is not available"
	SUDO="sudo"
	say "note: $INSTALL_DIR is not writable; will use sudo for the final copy only"
fi

ARCHIVE="mole_${NUM_VERSION}_${os}_${arch}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

say "mole $VERSION ($os/$arch) -> $INSTALL_DIR"

if [ "$DRY_RUN" = 1 ]; then
	say "dry run; would download:"
	say "  $BASE/$ARCHIVE"
	say "  $BASE/checksums.txt"
	exit 0
fi

# --- download and verify ------------------------------------------------------

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mole.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

curl -fsSL -o "$tmp/$ARCHIVE" "$BASE/$ARCHIVE" ||
	die "could not download $ARCHIVE — check that $VERSION exists for $os/$arch"
curl -fsSL -o "$tmp/checksums.txt" "$BASE/checksums.txt" ||
	die "could not download checksums.txt; refusing to install an unverified binary"

# Verification is not optional and there is no flag to skip it. The whole point
# of a checksum published beside the artifact is that the download can be wrong —
# truncated, cached by a proxy, or substituted — and the script cannot tell which.
expected=$(grep " $ARCHIVE\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || die "no checksum published for $ARCHIVE"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$ARCHIVE" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/$ARCHIVE" | awk '{print $1}')
else
	die "neither sha256sum nor shasum is available; cannot verify the download"
fi

if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for $ARCHIVE
  expected $expected
  actual   $actual
This is either a corrupted download or a tampered one. Nothing was installed."
fi
say "checksum ok"

# --- install ------------------------------------------------------------------

tar -xzf "$tmp/$ARCHIVE" -C "$tmp"

for bin in mole mole-mcp; do
	[ -f "$tmp/$bin" ] || die "$bin missing from the archive"
	chmod +x "$tmp/$bin"
	$SUDO cp "$tmp/$bin" "$INSTALL_DIR/$bin" || die "could not install $bin into $INSTALL_DIR"
done

say "installed mole and mole-mcp into $INSTALL_DIR"

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	say ""
	say "$INSTALL_DIR is not on your PATH. Add it:"
	say "  export PATH=\"\$PATH:$INSTALL_DIR\""
	;;
esac

say ""
say "next: mole config set search.provider tavily && mole doctor"
