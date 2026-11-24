#!/bin/sh
# bgraph installer for Linux and macOS:
#   curl -fsSL https://raw.githubusercontent.com/qxbao/bgraph/main/scripts/install.sh | sh
#
# Downloads the release archive for this OS/arch, checks its SHA-256 against checksums.txt, installs the binary and
# puts its directory on PATH (in the shell profile) when it is not there yet.
#
# Environment:
#   BGRAPH_VERSION      release to install, e.g. 0.2.0 (default: the latest)
#   BGRAPH_INSTALL_DIR  directory for the binary (default: $HOME/.local/bin)
#   BGRAPH_NO_MODIFY_PATH=1  leave the shell profile alone
set -eu

repo="https://github.com/qxbao/bgraph"
dir="${BGRAPH_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf 'bgraph-install: %s\n' "$*"; }
die() { printf 'bgraph-install: error: %s\n' "$*" >&2; exit 1; }

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
  latest_url() { curl -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q "$1" -O "$2"; }
  latest_url() { wget -q -S --spider "$repo/releases/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r'; }
else
  die "curl or wget is required"
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows run: irm https://raw.githubusercontent.com/qxbao/bgraph/main/scripts/install.ps1 | iex" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

v="${BGRAPH_VERSION:-}"
v="${v#v}"
if [ -z "$v" ]; then
  v="$(latest_url | sed 's|.*/tag/v||')"
  case "$v" in "" | */*) die "could not find the latest release" ;; esac
fi

asset="bgraph_${v}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading bgraph $v ($os/$arch)"
fetch "$repo/releases/download/v$v/$asset" "$tmp/$asset" || die "download failed: $repo/releases/download/v$v/$asset"
fetch "$repo/releases/download/v$v/checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums.txt"

want="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | awk '{ print $1 }')"
else
  got="$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')"
fi
[ "$got" = "$want" ] || die "SHA-256 mismatch for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" bgraph
mkdir -p "$dir" 2>/dev/null || die "cannot create $dir; set BGRAPH_INSTALL_DIR to a writable directory"
[ -w "$dir" ] || die "$dir is not writable; set BGRAPH_INSTALL_DIR to a writable directory"
# Write next to the target, then rename: replaces a running binary safely.
cp "$tmp/bgraph" "$dir/.bgraph.new"
chmod 755 "$dir/.bgraph.new"
mv -f "$dir/.bgraph.new" "$dir/bgraph"
say "installed $dir/bgraph"

case ":$PATH:" in
  *":$dir:"*) on_path=1 ;;
  *) on_path=0 ;;
esac
if [ "$on_path" = 0 ] && [ "${BGRAPH_NO_MODIFY_PATH:-}" != 1 ]; then
  case "$(basename "${SHELL:-sh}")" in
    zsh) profile="$HOME/.zshrc" line="export PATH=\"$dir:\$PATH\"" ;;
    bash)
      if [ "$os" = darwin ]; then profile="$HOME/.bash_profile"; else profile="$HOME/.bashrc"; fi
      line="export PATH=\"$dir:\$PATH\"" ;;
    fish) profile="$HOME/.config/fish/conf.d/bgraph.fish" line="fish_add_path \"$dir\"" ;;
    *) profile="$HOME/.profile" line="export PATH=\"$dir:\$PATH\"" ;;
  esac
  mkdir -p "$(dirname "$profile")"
  if ! grep -qsF "$line" "$profile"; then
    printf '\n# bgraph\n%s\n' "$line" >>"$profile"
    say "added $dir to PATH in $profile"
  fi
  say "open a new shell (or run: $line) to use bgraph"
fi

"$dir/bgraph" version
