#!/usr/bin/env bash

set -o errexit -o nounset -o pipefail

readonly TAG="${1:?release tag is required}"
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "release tag must be a semantic version prefixed with v: $TAG" >&2
  exit 1
fi

if [[ ! -f LICENSE && ! -f LICENSE.txt && ! -f LICENSE.md ]]; then
  echo "a repository license is required before publishing a release" >&2
  exit 1
fi

readonly VERSION="${TAG#v}"
readonly PREFIX="jvm_image-${VERSION}"
readonly ARCHIVE="jvm_image-${TAG}.tar.gz"

git archive --format=tar --prefix="${PREFIX}/" "$TAG" | gzip -n >"$ARCHIVE"
SHA256="$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')"
readonly SHA256

cat <<EOF
## Bzlmod

Add the module dependency to your \`MODULE.bazel\`:

\`\`\`starlark
bazel_dep(name = "jvm_image", version = "${VERSION}")
\`\`\`

Release archive SHA-256: \`${SHA256}\`
EOF
