#!/bin/sh
# Install mcpload on Linux or macOS:
#
#   curl -fsSL https://raw.githubusercontent.com/atul121001/mcpload/main/install.sh | sh
#
# Downloads the release archive for this computer from GitHub, checks its
# SHA-256 against the release's checksums.txt, unpacks it to
# ~/.mcpload/<version>/ (with ~/.mcpload/current pointing at it) and links
# `mcpload` into a folder on your PATH. Running it again upgrades. No sudo.
#
# Settings (environment variables):
#   MCPLOAD_VERSION      release to install, e.g. v0.5.0 (default: latest)
#   MCPLOAD_INSTALL_DIR  where releases are unpacked (default: ~/.mcpload)
#   MCPLOAD_BIN_DIR      where the `mcpload` link goes (default: ~/.local/bin,
#                        or /usr/local/bin if it is writable and ~/.local/bin
#                        is not on your PATH)
#   MCPLOAD_NO_PATH=1    don't create the link (just unpack)
#
# Everything is inside main(), so a partly downloaded script does nothing.

set -eu

REPO="atul121001/mcpload"

say() { printf '%s\n' "$*"; }
err() {
	printf 'mcpload install: error: %s\n' "$*" >&2
	exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

# fetch URL FILE downloads URL to FILE (FILE "-" = stdout).
fetch() {
	if has curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif has wget; then
		wget -q -O "$2" "$1"
	else
		err "need curl or wget to download mcpload"
	fi
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		err "this script is for Linux and macOS; on Windows run in PowerShell: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
	*) err "unsupported operating system $(uname -s); download a release by hand from https://github.com/$REPO/releases" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	# A shell running under Rosetta on Apple silicon reports x86_64.
	if [ "$1" = darwin ] && [ "$arch" = x86_64 ] &&
		[ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
	case "$arch" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64 | armv8*) echo arm64 ;;
	*) err "unsupported CPU $arch (releases exist for amd64 and arm64); see https://github.com/$REPO/releases" ;;
	esac
}

# latest_tag prints the newest release tag, e.g. v0.5.0.
latest_tag() {
	tag="$(fetch "https://api.github.com/repos/$REPO/releases/latest" - 2>/dev/null |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)" || tag=""
	if [ -z "$tag" ] && has curl; then
		# API rate-limited: follow the /releases/latest redirect instead.
		url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null)" || url=""
		case "$url" in
		*/tag/*) tag="${url##*/tag/}" ;;
		esac
	fi
	[ -n "$tag" ] || err "could not find the latest release (GitHub API unreachable?); set MCPLOAD_VERSION=v0.5.0 to pick one"
	echo "$tag"
}

sha256_of() {
	if has sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif has shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		err "need sha256sum or shasum to verify the download; refusing to install unverified files"
	fi
}

on_path() {
	case ":${PATH:-}:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

main() {
	for c in uname tar mkdir ln awk sed mktemp; do
		has "$c" || err "required command '$c' not found"
	done

	os="$(detect_os)"
	arch="$(detect_arch "$os")"

	tag="${MCPLOAD_VERSION:-}"
	if [ -z "$tag" ]; then
		tag="$(latest_tag)"
	fi
	case "$tag" in
	v*) ;;
	*) tag="v$tag" ;;
	esac
	ver="${tag#v}"
	name="mcpload_${ver}_${os}_${arch}"
	archive="$name.tar.gz"
	base="https://github.com/$REPO/releases/download/$tag"

	root="${MCPLOAD_INSTALL_DIR:-$HOME/.mcpload}"
	dest="$root/$tag"

	tmp="$(mktemp -d 2>/dev/null || mktemp -d -t mcpload)"
	# shellcheck disable=SC2064 # expand $tmp now
	trap "rm -rf '$tmp'" EXIT
	trap 'exit 1' INT TERM

	say "Installing mcpload $tag ($os/$arch)"
	say "  downloading $base/$archive"
	fetch "$base/$archive" "$tmp/$archive" ||
		err "download failed: $base/$archive (does release $tag exist? see https://github.com/$REPO/releases)"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" ||
		err "download failed: $base/checksums.txt"

	want="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
	[ -n "$want" ] || err "checksums.txt of $tag has no entry for $archive"
	got="$(sha256_of "$tmp/$archive")"
	if [ "$got" != "$want" ]; then
		err "checksum mismatch for $archive (expected $want, got $got); the download is corrupt or was tampered with"
	fi
	say "  verified SHA-256 $got"

	mkdir -p "$root" || err "cannot create $root (set MCPLOAD_INSTALL_DIR to a writable folder)"
	stage="$root/.$name.tmp.$$"
	rm -rf "$stage"
	mkdir -p "$stage"
	tar -xzf "$tmp/$archive" -C "$stage" || {
		rm -rf "$stage"
		err "could not unpack $archive"
	}
	[ -f "$stage/$name/mcpload" ] || {
		rm -rf "$stage"
		err "$archive does not contain $name/mcpload"
	}
	chmod 755 "$stage/$name/mcpload"
	if [ "$os" = darwin ] && has xattr; then
		# Let Gatekeeper run the binaries without the "cannot be verified" prompt.
		xattr -dr com.apple.quarantine "$stage/$name" 2>/dev/null || true
	fi
	rm -rf "$dest"
	mv "$stage/$name" "$dest"
	rm -rf "$stage"

	if [ -e "$root/current" ] && [ ! -L "$root/current" ]; then
		err "$root/current exists and is not a symlink; remove it and run the installer again"
	fi
	ln -sfn "$tag" "$root/current"

	# Remove older versions installed by this script.
	for old in "$root"/v*; do
		if [ -d "$old" ] && [ "$old" != "$dest" ] && [ -f "$old/mcpload" ]; then
			rm -rf "$old"
		fi
	done

	say "  installed to $dest"

	link=""
	if [ "${MCPLOAD_NO_PATH:-}" != 1 ]; then
		bindir="${MCPLOAD_BIN_DIR:-}"
		if [ -z "$bindir" ]; then
			bindir="$HOME/.local/bin"
			if ! on_path "$bindir" && [ -d /usr/local/bin ] && [ -w /usr/local/bin ] && on_path /usr/local/bin; then
				bindir=/usr/local/bin
			fi
		fi
		mkdir -p "$bindir" || err "cannot create $bindir (set MCPLOAD_BIN_DIR to a writable folder)"
		link="$bindir/mcpload"
		if [ -e "$link" ] && [ ! -L "$link" ]; then
			say "  replacing $link (was a regular file)"
			rm -f "$link"
		fi
		ln -sf "$root/current/mcpload" "$link"
		say "  linked $link -> $root/current/mcpload"
	fi

	say ""
	"$dest/mcpload" version || err "the installed mcpload does not run on this computer"
	say ""

	if [ -z "$link" ]; then
		say "Done. Run it as: $dest/mcpload"
		return
	fi
	if on_path "$bindir"; then
		say "Done. Try it:"
		say "  mcpload run --url <your MCP server URL>"
		return
	fi
	case "${SHELL:-}" in
	*/zsh) rc="$HOME/.zshrc" ;;
	*/bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
	*/fish) rc="" ;;
	*) rc="$HOME/.profile" ;;
	esac
	say "Done. $bindir is not on your PATH yet. Add it with:"
	if [ -n "$rc" ]; then
		say "  echo 'export PATH=\"$bindir:\$PATH\"' >> $rc && . $rc"
	else
		say "  fish_add_path $bindir"
	fi
	say "then try it:"
	say "  mcpload run --url <your MCP server URL>"
}

main "$@"
