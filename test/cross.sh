#!/bin/sh
# Vet and build every package for each supported GOOS/GOARCH, and check the go directive.
set -eu
cd "$(dirname "$0")/.."

# The go directive must be the highest go version any dependency declares (spec 2).
want=$(go list -m -f '{{if not .Main}}{{.GoVersion}}{{end}}' all | grep . | sort -V | tail -n 1)
have=$(go list -m -f '{{.GoVersion}}')
if [ -n "$want" ] && [ "$have" != "$want" ]; then
	echo "go.mod says go $have, but the dependencies need go $want" >&2
	exit 1
fi

fail=0
for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os=${pair%/*}
	arch=${pair#*/}
	echo "== $os/$arch"
	if ! CGO_ENABLED=0 GOOS=$os GOARCH=$arch go vet ./... ||
		! CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build ./...; then
		echo "FAIL $os/$arch" >&2
		fail=1
	fi
done
exit $fail
