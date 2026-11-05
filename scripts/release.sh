#!/usr/bin/env bash
# release.sh tags, builds and publishes a release of importgz, following docs/RELEASING.md.
set -euo pipefail

REPO=tb12as/import-gz
TARGETS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)

die() {
  echo "error: $*" >&2
  exit 1
}

cd "$(git rev-parse --show-toplevel)"

for c in go gh git zip tar sha256sum; do
  command -v "$c" >/dev/null || die "$c is not installed"
done
gh auth status >/dev/null 2>&1 || die "gh is not authenticated; run: gh auth login"

# 1. the release must be built from a clean main that matches github
[ "$(git branch --show-current)" = main ] || die "not on main"
git fetch -q --tags origin

# untracked files count too: go build would stamp the binary +dirty
[ -z "$(git status --porcelain)" ] || die "working tree is not clean:
$(git status --short)"
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "main does not match origin/main; pull or push first"

echo "Running vet and tests..."
go vet ./...
go test -count=1 ./...

# 2. version: suggest the next minor
PREV=$(git describe --tags --abbrev=0 2>/dev/null || true)
SUG=""
if [[ $PREV =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+$ ]]; then
  SUG="v${BASH_REMATCH[1]}.$((BASH_REMATCH[2] + 1)).0"
  [ "$(git rev-list -n1 "$PREV")" != "$(git rev-parse HEAD)" ] || die "HEAD is already released as $PREV"
fi

read -rp "Version (latest: ${PREV:-none}) [$SUG]: " V
V=${V:-$SUG}

[[ $V =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must look like v1.2.3"

# go ignores v2+ tags unless the module path in go.mod ends in /v2
[[ $V =~ ^v[01]\. ]] || die "$V needs the module path in go.mod changed to .../${V%%.*} first"
! git rev-parse -q --verify "refs/tags/$V" >/dev/null || die "tag $V already exists"
if [ -n "$PREV" ] && [ "$(printf '%s\n' "$PREV" "$V" | sort -V | tail -1)" != "$V" ]; then
  die "$V is not newer than $PREV"
fi

# 3. tag locally first; until it is pushed, any failure removes it again
git tag -a "$V" -m "$V"
pushed=0
done=0
D=$(mktemp -d)

on_exit() {
  if [ $pushed = 0 ]; then
    git tag -d "$V" >/dev/null 2>&1 || true
    echo "Nothing was published; the local tag $V was removed." >&2
  elif [ $done = 0 ]; then
    echo "Tag $V is pushed but the release was not created. Retry with:" >&2
    echo "  gh release create $V -R $REPO --verify-tag --title $V --notes-file $D/notes.md \\" >&2
    echo "    $D/*.tar.gz $D/*.zip $D/checksums.txt" >&2
  fi
}
trap on_exit EXIT

echo "Building into $D..."
for t in "${TARGETS[@]}"; do
  os=${t%/*}
  arch=${t#*/}
  n=importgz_${V}_${os}_${arch}
  ext=""
  [ "$os" = windows ] && ext=.exe

  mkdir -p "$D/$n"
  cp README.md "$D/$n/"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$D/$n/importgz$ext" ./cmd/importgz

  # a +dirty or pseudo-version here means the tree or the tag was wrong
  got=$(go version -m "$D/$n/importgz$ext" | awk '$1 == "mod" { print $3 }')
  [ "$got" = "$V" ] || die "$n reports version $got, want $V"

  if [ "$os" = windows ]; then
    (cd "$D" && zip -qr "$n.zip" "$n")
  else
    (cd "$D" && tar czf "$n.tar.gz" "$n")
  fi
  rm -rf "${D:?}/$n"
done
(cd "$D" && sha256sum -- *.tar.gz *.zip >checksums.txt)

# 4. notes: draft from the Added/Changed/Fixed/Removed bullets of every commit since PREV
{
  printf '## Changes\n\n'
  git log --reverse --format=%b "${PREV:+$PREV..}$V" |
    awk '/^- (Added|Changed|Fixed|Removed):/ { p = 1; print; next } p && /^  / { print; next } { p = 0 }'
  printf '\n## Installation\n\n'
  printf 'Download the archive for your platform below, extract it, and place `importgz` on your `PATH`. '
  printf 'Alternatively, install from source:\n\n'
  printf '```sh\ngo install github.com/%s/cmd/importgz@%s\n```\n\n' "$REPO" "$V"
  printf 'Verify downloads against `checksums.txt`.\n'
  if [ -n "$PREV" ]; then
    printf '\n**Full changelog:** https://github.com/%s/compare/%s...%s\n' "$REPO" "$PREV" "$V"
  fi
} >"$D/notes.md"

read -rp "Release notes drafted in $D/notes.md. Press Enter to edit them..."
"${EDITOR:-vi}" "$D/notes.md"

echo
cat "$D/notes.md"
echo
ls "$D"
echo
read -rp "Push tag $V and publish the release? [y/N] " a
[[ $a =~ ^[Yy]([Ee][Ss])?$ ]] || die "aborted"

# 5. publish
git push origin "$V"
pushed=1
gh release create "$V" -R "$REPO" --verify-tag --title "$V" --notes-file "$D/notes.md" \
  "$D"/*.tar.gz "$D"/*.zip "$D/checksums.txt"
done=1

# 6. verify; failures here are warnings because the release already exists
want=$((${#TARGETS[@]} + 1))
got=$(gh release view "$V" -R "$REPO" --json assets --jq '.assets | length')
[ "$got" = "$want" ] || echo "warning: the release has $got files, want $want" >&2

G=$(mktemp -d)
if GOBIN=$G go install "github.com/$REPO/cmd/importgz@$V"; then
  "$G/importgz" -version
else
  echo "warning: go install @$V failed; the module proxy can take a few minutes to see a new tag" >&2
fi
rm -rf "$G"

echo "Released $V: https://github.com/$REPO/releases/tag/$V"
