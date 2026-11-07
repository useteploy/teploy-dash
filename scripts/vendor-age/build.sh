#!/bin/sh
# Reproduces cmd/teploy-dash/frontend/js/age.vendor.js from npm. Needs node and network.
# The bundle is committed; nothing runs this at Dash build time (Dash has no build step).
set -eu
AGE_VERSION=0.3.1
ESBUILD_VERSION=0.25.12
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$here/entry.mjs" "$work/entry.mjs"
cd "$work"
npm init -y >/dev/null
npm install --no-audit --no-fund --save-exact "age-encryption@$AGE_VERSION" "esbuild@$ESBUILD_VERSION" >/dev/null
npx esbuild entry.mjs --bundle --format=iife --platform=browser --target=es2020 --minify --legal-comments=none \
  --outfile="$root/cmd/teploy-dash/frontend/js/age.vendor.js"
cp node_modules/age-encryption/LICENSE "$root/cmd/teploy-dash/frontend/js/age.vendor.LICENSE"
