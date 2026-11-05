# Releasing importgz

This document describes how to publish a new version of `importgz` on GitHub. A release consists of a git
tag, prebuilt archives for six platforms, a checksum file, and release notes.

## Prerequisites

- Go 1.27 or newer.
- The GitHub CLI (`gh`), authenticated with write access to `tb12as/import-gz` (`gh auth status`).
- `zip`, `tar`, and `sha256sum`.
- A clean working tree on `main` that matches `origin/main`.

## How the version is determined

The binary has no version constant. `importgz -version` reads the version that the Go toolchain records at
build time:

| Build                                             | Reported version                   |
|---------------------------------------------------|------------------------------------|
| `go install github.com/tb12as/import-gz/cmd/importgz@v0.2.0` | `v0.2.0`                |
| `go build` in a clean checkout of a tagged commit | `v0.2.0`                           |
| `go build` with uncommitted changes               | `v0.2.0+dirty`                     |
| `go build` on commits after a tag                 | a pseudo-version, such as `v0.2.1-0.20261104…-28ae43e` |
| `go build` without git metadata                   | `(devel)`                          |

For this reason, the tag must exist **before** the release binaries are built, and the working tree must be
clean when they are built.

## Choosing a version number

Versions follow [Semantic Versioning](https://semver.org/) with a `v` prefix:

- Increase the **minor** version (`v0.1.1` to `v0.2.0`) for new features, such as a new flag.
- Increase the **patch** version (`v0.2.0` to `v0.2.1`) for bug fixes only.
- Increase the **major** version only for incompatible changes to flags or behavior, after `v1.0.0`.

To list existing versions:

```sh
gh release list -R tb12as/import-gz
git tag -l
```

## Steps

The commands below use `V` for the new version. Run them from the repository root.

### 1. Verify the state of the repository

```sh
V=v0.2.0

git switch main
git fetch origin
git status -sb                 # must show "## main...origin/main" with no changes
go vet ./...
go test -count=1 ./...
```

If `main` is ahead of `origin/main`, push it first with `git push origin main`. A release must point at a
commit that exists on GitHub.

### 2. Create and push the tag

```sh
git tag -a "$V" -m "$V"
git push origin "$V"
```

### 3. Build the archives

The archives are written to a temporary directory so that build output never enters the repository.

```sh
D=$(mktemp -d)

for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${t%/*}; arch=${t#*/}
  n=importgz_${V}_${os}_${arch}
  ext=""; [ "$os" = windows ] && ext=.exe

  mkdir -p "$D/$n" && cp README.md "$D/$n/"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
    go build -trimpath -ldflags="-s -w" -o "$D/$n/importgz$ext" ./cmd/importgz || break

  (cd "$D" && if [ "$os" = windows ]; then zip -qr "$n.zip" "$n"; else tar czf "$n.tar.gz" "$n"; fi && rm -rf "$n")
done

(cd "$D" && sha256sum *.tar.gz *.zip > checksums.txt)
ls "$D"
```

The build flags have the following purpose:

- `CGO_ENABLED=0` produces static binaries that do not depend on the C library of the target system.
- `-trimpath` removes local file system paths from the binary.
- `-ldflags="-s -w"` removes debug information, which reduces the binary size.

The directory must contain six archives and `checksums.txt`.

### 4. Verify the binaries

Confirm that the binaries report the new version. A `+dirty` suffix or a pseudo-version means that the tree was
not clean or that the tag was missing during the build; in that case, fix the cause and rebuild.

```sh
tar xzf "$D/importgz_${V}_linux_amd64.tar.gz" -C "$D"
"$D/importgz_${V}_linux_amd64/importgz" -version
rm -rf "$D/importgz_${V}_linux_amd64"

# binaries for other platforms can be inspected without running them
unzip -qo "$D/importgz_${V}_windows_amd64.zip" -d "$D"
go version -m "$D/importgz_${V}_windows_amd64/importgz.exe" | grep -E '^\s+mod\s'
rm -rf "$D/importgz_${V}_windows_amd64"
```

### 5. Write the release notes

Create the notes in the temporary directory. List each change on its own line, prefixed with `Added:`,
`Changed:`, `Fixed:`, or `Removed:`, and written in the past tense. Use `git log` to collect the changes since
the previous release:

```sh
PREV=$(git describe --tags --abbrev=0 "$V^")
git log --oneline "$PREV..$V"
```

Example `"$D/notes.md"`:

````markdown
## Changes

- Added: the `-parallel` flag, which loads the tables of a dump in concurrent sessions.

## Installation

Download the archive for your platform below, extract it, and place `importgz` on your `PATH`. Alternatively,
install from source:

```sh
go install github.com/tb12as/import-gz/cmd/importgz@v0.2.0
```

Verify downloads against `checksums.txt`.

**Full changelog:** https://github.com/tb12as/import-gz/compare/v0.1.1...v0.2.0
````

### 6. Publish the release

```sh
gh release create "$V" -R tb12as/import-gz --verify-tag --title "$V" --notes-file "$D/notes.md" \
  "$D"/*.tar.gz "$D"/*.zip "$D/checksums.txt"
```

`--verify-tag` makes the command fail if the tag does not exist on GitHub, instead of creating a new tag
on the current state of `main`.

### 7. Verify the published release

```sh
gh release view "$V" -R tb12as/import-gz --json assets --jq '.assets[].name'   # seven files

G=$(mktemp -d)
GOBIN=$G go install "github.com/tb12as/import-gz/cmd/importgz@$V"
"$G/importgz" -version
```

The release is complete when both commands succeed and the installed binary reports the new version.

## Correcting a release

- **Wrong release notes or missing archives:** edit the release with `gh release edit "$V" --notes-file ...`,
  or upload files with `gh release upload "$V" <files> --clobber`. The tag does not change.
- **Wrong code in the tagged commit:** do not move or reuse the tag. The Go module proxy and checksum database
  (`proxy.golang.org`, `sum.golang.org`) store each version permanently the first time it is requested, so a
  retagged version fails checksum verification for users who already downloaded it. Publish a new patch version
  with the fix instead.
- **A release published by mistake, before anyone used it:** delete it with
  `gh release delete "$V" -R tb12as/import-gz --cleanup-tag` and `git tag -d "$V"`. Even then, choose a new
  version number for the next attempt if the version may already have been fetched through the module proxy.
