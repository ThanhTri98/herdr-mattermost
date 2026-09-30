#!/bin/sh
# Build herdr-mm with Go 1.25+ when present, else download the prebuilt
# binary of this exact commit from its GitHub release (see .github/workflows/release.yml).
set -eu
repo=ThanhTri98/herdr-mattermost

v=$(go version 2>/dev/null | sed -n 's/.*go\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p') || v=
if [ -n "$v" ]; then
	# shellcheck disable=SC2086 # split "major minor"
	set -- $v
	if [ "$1" -gt 1 ] || { [ "$1" -eq 1 ] && [ "$2" -ge 25 ]; }; then
		exec go build -o herdr-mm .
	fi
fi

case $(uname -s) in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) echo "herdr-mm: unsupported OS $(uname -s); install Go 1.25+ to build it" >&2; exit 1 ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "herdr-mm: unsupported architecture $(uname -m); install Go 1.25+ to build it" >&2; exit 1 ;;
esac

sha=$(git rev-parse HEAD)
url="https://github.com/$repo/releases/download/build-$sha/herdr-mm-$os-$arch"
if ! command -v curl >/dev/null 2>&1; then
	echo "herdr-mm: no Go 1.25+ or curl; install Go 1.25+ to build it, or curl to download it" >&2
	exit 1
fi
if ! curl -fsSL -o herdr-mm.tmp "$url"; then
	rm -f herdr-mm.tmp
	echo "herdr-mm: no prebuilt binary for commit $sha ($os-$arch) at $url; install Go 1.25+, or retry once the release for this commit is published" >&2
	exit 1
fi
chmod +x herdr-mm.tmp
mv herdr-mm.tmp herdr-mm
