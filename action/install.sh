#!/usr/bin/env bash
# Installs bgraph for the composite action (action.yml): downloads the release archive for this runner, verifies it
# against the release's checksums.txt and adds it to PATH. With BGRAPH_BINARY set, uses that local binary instead.
#
# Environment: BGRAPH_VERSION (latest | vX.Y.Z | X.Y.Z), BGRAPH_BINARY (optional), BGRAPH_REPO (owner/name).
set -euo pipefail

err() { echo "::error title=bgraph::$*"; exit 1; }

# native converts a POSIX path to the runner's native form (Windows paths for GITHUB_PATH and outputs).
native() {
  if [ "${RUNNER_OS:-}" = "Windows" ] && command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}
posix() {
  if [ "${RUNNER_OS:-}" = "Windows" ] && command -v cygpath >/dev/null 2>&1; then cygpath -u "$1"; else printf '%s' "$1"; fi
}

# download URL FILE: some curl versions exit 0 on an HTTP error when --retry is given, so check the file as well.
download() {
  rm -f "$2"
  curl -fsSL --retry 3 -o "$2" "$1" && [ -s "$2" ]
}

exe=bgraph
[ "${RUNNER_OS:-}" = "Windows" ] && exe=bgraph.exe

if [ -n "${BGRAPH_BINARY:-}" ]; then
  bin=$(posix "$BGRAPH_BINARY")
  [ -f "$bin" ] || err "binary not found: $BGRAPH_BINARY"
  dir=$(cd "$(dirname "$bin")" && pwd)
  bin="$dir/$(basename "$bin")"
else
  case "${RUNNER_OS:-}" in
    Linux) os=linux ;;
    macOS) os=darwin ;;
    Windows) os=windows ;;
    *) err "unsupported runner OS: ${RUNNER_OS:-unknown}" ;;
  esac
  case "${RUNNER_ARCH:-}" in
    X64) arch=amd64 ;;
    ARM64) arch=arm64 ;;
    *) err "unsupported runner architecture: ${RUNNER_ARCH:-unknown}" ;;
  esac

  repo=${BGRAPH_REPO:-qxbao/bgraph}
  version=${BGRAPH_VERSION:-latest}
  if [ "$version" = "latest" ]; then
    # The web redirect of releases/latest names the tag; it needs no token and has no API rate limit.
    url=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
      err "cannot resolve the latest release of $repo"
    tag=${url##*/tag/}
    [ "$tag" != "$url" ] && [ -n "$tag" ] || err "no release found for $repo"
  else
    tag="v${version#v}"
  fi
  ver=${tag#v}

  ext=tar.gz
  [ "$os" = "windows" ] && ext=zip
  asset="bgraph_${ver}_${os}_${arch}.${ext}"
  base="https://github.com/$repo/releases/download/$tag"

  dir="$(posix "${RUNNER_TEMP:-${TMPDIR:-/tmp}}")/bgraph-$ver-$os-$arch"
  rm -rf "$dir"
  mkdir -p "$dir"
  echo "Downloading $asset ($tag)"
  download "$base/$asset" "$dir/$asset" || err "cannot download $base/$asset (does release $tag exist?)"
  download "$base/checksums.txt" "$dir/checksums.txt" || err "cannot download $base/checksums.txt"

  want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$dir/checksums.txt")
  [ -n "$want" ] || err "$asset is not listed in checksums.txt"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$dir/$asset" | awk '{ print $1 }')
  else
    got=$(shasum -a 256 "$dir/$asset" | awk '{ print $1 }')
  fi
  [ "$got" = "$want" ] || err "checksum mismatch for $asset: got $got, want $want"
  echo "Checksum OK ($got)"

  if [ "$ext" = "zip" ]; then
    if command -v unzip >/dev/null 2>&1; then
      unzip -q -o "$dir/$asset" -d "$dir"
    else
      pwsh -NoProfile -Command "Expand-Archive -Force -LiteralPath '$(native "$dir/$asset")' -DestinationPath '$(native "$dir")'"
    fi
  else
    tar -xzf "$dir/$asset" -C "$dir"
  fi
  rm -f "$dir/$asset"
  bin="$dir/$exe"
  [ -f "$bin" ] || err "$exe not found in $asset"
  chmod +x "$bin"
fi

"$bin" version
echo "$(native "$dir")" >>"$GITHUB_PATH"
echo "path=$(native "$bin")" >>"$GITHUB_OUTPUT"
