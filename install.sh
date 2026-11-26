#!/bin/sh
# Install a prebuilt nuncio binary.
#
#   curl -fsSL https://raw.githubusercontent.com/NoahIsARider/nuncio/main/install.sh | sh
#
# Environment:
#   VERSION      release tag, e.g. v0.1.0 (default: latest)
#   INSTALL_DIR  destination directory (default: /usr/local/bin when writable,
#                otherwise $HOME/.local/bin)
set -eu

REPO="NoahIsARider/nuncio"
BIN="nuncio"
VERSION="${VERSION:-latest}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install: $*"; exit 1; }

case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
	linux) os=linux ;;
	darwin) os=darwin ;;
	mingw* | msys* | cygwin*) os=windows ;;
	*) die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l) arch=armv7 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
esac

if [ "$VERSION" = "latest" ]; then
	base="https://github.com/${REPO}/releases/latest/download"
else
	base="https://github.com/${REPO}/releases/download/${VERSION}"
fi
asset="${BIN}_${os}_${arch}.tar.gz"
url="${base}/${asset}"

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		die "need curl or wget"
	fi
}

tmp="$(mktemp -d "${TMPDIR:-/tmp}/nuncio.XXXXXX")" || die "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT INT TERM
# Under MSYS/git-bash the native curl.exe cannot write to the /tmp path mktemp
# returns, while MSYS tar cannot read a C:/ path (it parses the drive as a
# remote host). Keep both spellings of the same directory.
native_tmp="$tmp"
if command -v cygpath >/dev/null 2>&1; then
	native_tmp="$(cygpath -m "$tmp")"
fi

say "downloading ${url}"
if command -v curl >/dev/null 2>&1; then
	curl -fsSL "$url" -o "$native_tmp/$asset" || die "download failed"
else
	wget -qO "$native_tmp/$asset" "$url" || die "download failed"
fi

if command -v sha256sum >/dev/null 2>&1; then
	# sha256sum writes "hash  file" in text mode and "hash *file" in binary
	# mode (the Windows default), so strip the asterisk before comparing.
	sums="$(fetch "${base}/checksums.txt" 2>/dev/null || true)"
	want="$(printf '%s\n' "$sums" | awk -v a="$asset" '{ f=$2; sub(/^\*/, "", f); if (f == a) { print $1; exit } }')"
	if [ -n "${want:-}" ]; then
		got="$(sha256sum "$tmp/$asset" | awk '{ print $1 }')"
		[ "$want" = "$got" ] || die "checksum mismatch for $asset"
		say "checksum ok"
	elif [ -z "${sums:-}" ]; then
		say "warning: could not fetch checksums.txt; installing WITHOUT verification"
	else
		say "warning: checksums.txt does not list $asset; installing WITHOUT verification"
	fi
fi

tar -xzf "$tmp/$asset" -C "$tmp" || die "cannot extract $asset"
if [ -f "$tmp/$BIN" ]; then
	src="$tmp/$BIN"
elif [ -f "$tmp/$BIN.exe" ]; then
	src="$tmp/$BIN.exe"
else
	die "archive did not contain $BIN"
fi

if [ -n "${INSTALL_DIR:-}" ]; then
	dir="$INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi
mkdir -p "$dir" || die "cannot create $dir"
dest="$dir/$BIN"
if [ "$os" = windows ]; then dest="$dest.exe"; fi

if command -v install >/dev/null 2>&1; then
	install -m 0755 "$src" "$dest" || die "cannot install to $dest"
else
	cp "$src" "$dest" || die "cannot copy to $dest"
	chmod 0755 "$dest" || true
fi

say "installed $dest"
case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "note: $dir is not on PATH" ;;
esac
"$dest" version || true
