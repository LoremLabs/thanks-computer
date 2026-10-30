#!/usr/bin/env sh
# Rebuild the vendored txco Javy plugin (see plugin/README.md).
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
version="$(sed -n 's/^const JavyVersion = "\(.*\)"$/\1/p' "$here/javyplugin.go")"
out="$here/txco-javy-plugin-v1.wasm"

javy="${TXCO_JAVY:-}"
if [ -z "$javy" ]; then
  for cand in "$(command -v javy || true)" "${XDG_CONFIG_HOME:-$HOME/.config}/txco/tools/javy-$version"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then javy="$cand"; break; fi
  done
fi
if [ -z "$javy" ]; then
  echo "build.sh: no javy $version found (set TXCO_JAVY, or run any 'txco op run' once to download it)" >&2
  exit 1
fi
have="$("$javy" --version | awk '{print $2}')"
if [ "$have" != "$version" ]; then
  echo "build.sh: $javy is javy $have, want $version (javyplugin.JavyVersion)" >&2
  exit 1
fi

cd "$here/plugin"
cargo build --target wasm32-wasip1 --release
"$javy" init-plugin target/wasm32-wasip1/release/plugin.wasm -o "$out"
ls -l "$out"
