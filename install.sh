#!/bin/sh
set -eu

version="${REMOTE_VERSION:-v0.1.0}"
install_dir="${REMOTE_INSTALL_DIR:-$HOME/.local/bin}"
repo="adrielGGmotion/actions-cli"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "remote: unsupported operating system" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "remote: unsupported architecture" >&2; exit 1 ;;
esac

archive="remote_${version}_${os}_${arch}.tar.gz"
base="https://github.com/${repo}/releases/download/${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

expected="$(awk -v file="$archive" '$2 == file { print $1 }' "$tmp/checksums.txt")"
test -n "$expected" || { echo "remote: checksum not found" >&2; exit 1; }

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
else
  echo "remote: sha256sum or shasum is required" >&2
  exit 1
fi

test "$actual" = "$expected" || { echo "remote: checksum verification failed" >&2; exit 1; }
tar -xzf "$tmp/$archive" -C "$tmp" remote
mkdir -p "$install_dir"
install -m 0755 "$tmp/remote" "$install_dir/remote"
echo "remote installed to $install_dir/remote"
