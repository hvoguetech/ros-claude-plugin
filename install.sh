#!/bin/sh
# Install the ros-local helper CLI from GitHub Releases, verifying its SHA-256 checksum.
#
#   curl -fsSL https://raw.githubusercontent.com/hvoguetech/ros-claude-plugin/main/install.sh | sh
#
# Options (environment):
#   ROS_LOCAL_VERSION       version to install, e.g. 0.1.0 (default: the latest release)
#   ROS_LOCAL_INSTALL_DIR   where to put the binary (default: ~/.local/bin)
#   ROS_LOCAL_REQUIRE_SIGNATURE=1
#                           fail unless cosign is installed and the checksums' signature verifies
#   ROS_LOCAL_DOWNLOAD_BASE  release download base (default: GitHub Releases; for internal mirrors)
#
# The checksum file itself is signed with cosign (keyless, by this repository's release workflow).
# If cosign is installed, the signature is verified too.
set -eu

REPO="hvoguetech/ros-claude-plugin"
BASE="${ROS_LOCAL_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}"
INSTALL_DIR="${ROS_LOCAL_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*" >&2; }
die() {
	say "install.sh: $*"
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }
need uname
need tar
need curl

os=$(uname -s)
case "$os" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported OS $os (ros-local supports macOS and Linux)" ;;
esac
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported CPU $arch (ros-local supports x86_64 and arm64)" ;;
esac

version="${ROS_LOCAL_VERSION:-}"
if [ -z "$version" ]; then
	# The redirect target of /releases/latest names the tag; no API token needed.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not find the latest release"
	version=${url##*/}
fi
version=${version#v}
case "$version" in
'' | *[!0-9A-Za-z.+-]*) die "bad version '$version'" ;;
esac

archive="ros-local_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading ros-local $version for $os/$arch…"
curl -fsSL -o "$tmp/$archive" "$BASE/v$version/$archive" || die "download failed: $BASE/v$version/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$BASE/v$version/checksums.txt" || die "could not download checksums.txt"

if command -v cosign >/dev/null 2>&1; then
	curl -fsSL -o "$tmp/checksums.txt.sigstore.json" "$BASE/v$version/checksums.txt.sigstore.json" ||
		die "could not download the checksum signature"
	cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "^https://github.com/$REPO/\\.github/workflows/release\\.yml@" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com \
		"$tmp/checksums.txt" >/dev/null 2>&1 || die "signature verification of checksums.txt FAILED"
	say "Signature verified (cosign)."
elif [ "${ROS_LOCAL_REQUIRE_SIGNATURE:-}" = 1 ]; then
	die "ROS_LOCAL_REQUIRE_SIGNATURE=1 but cosign is not installed"
fi

expected=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
else
	die "need sha256sum or shasum to verify the download"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive (expected $expected, got $actual)"
say "Checksum verified."

tar -xzf "$tmp/$archive" -C "$tmp" ros-local
mkdir -p "$INSTALL_DIR"
install_path="$INSTALL_DIR/ros-local"
cp "$tmp/ros-local" "$install_path.tmp"
chmod 0755 "$install_path.tmp"
mv -f "$install_path.tmp" "$install_path"
say "Installed $install_path"

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*) say "Note: $INSTALL_DIR is not on your PATH. Add it, e.g.: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say "Next: ros-local login"
