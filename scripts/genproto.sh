#!/usr/bin/env bash
# Regenerates the Go gRPC stubs in gen/ from proto/ (spec sections 11, 21.6, 22.9).
#
# Uses only local tools: buf for compiling/linting, protoc-gen-go and
# protoc-gen-go-grpc as local plugins (no remote buf.build plugins, no protoc).
# Tools are installed into $(go env GOPATH)/bin; this never touches go.mod.
#
# Usage: scripts/genproto.sh            # install tools if missing, lint, generate
#        SKIP_LINT=1 scripts/genproto.sh
set -euo pipefail

BUF_VERSION="${BUF_VERSION:-v1.75.0}"
PROTOC_GEN_GO_VERSION="${PROTOC_GEN_GO_VERSION:-v1.36.12}"  # keep equal to google.golang.org/protobuf in go.mod
PROTOC_GEN_GO_GRPC_VERSION="${PROTOC_GEN_GO_GRPC_VERSION:-v1.6.2}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

gobin="$(go env GOBIN)"
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
export PATH="$gobin:$PATH"

# install <binary> <module path> <version> <version string printed by --version>
install() {
  local bin="$1" pkg="$2" ver="$3" want="$4"
  if ! command -v "$bin" >/dev/null 2>&1 || ! "$bin" --version 2>&1 | grep -qF "$want"; then
    echo "installing $pkg@$ver" >&2
    (cd / && GOFLAGS= go install "$pkg@$ver")
  fi
}
install buf github.com/bufbuild/buf/cmd/buf "$BUF_VERSION" "${BUF_VERSION#v}"
install protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$PROTOC_GEN_GO_VERSION" "$PROTOC_GEN_GO_VERSION"
install protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc "$PROTOC_GEN_GO_GRPC_VERSION" "${PROTOC_GEN_GO_GRPC_VERSION#v}"

if [ -z "${SKIP_LINT:-}" ]; then
  buf lint
fi

# Remove previously generated files (hand-written *_test.go files in gen/ stay).
if [ -d gen ]; then
  find gen -type f \( -name '*.pb.go' -o -name '*_grpc.pb.go' \) -delete
fi

buf generate
gofmt -l gen | (! grep .) || { echo "generated code is not gofmt-clean" >&2; exit 1; }
echo "generated: $(find gen -name '*.pb.go' | wc -l) .pb.go files" >&2
