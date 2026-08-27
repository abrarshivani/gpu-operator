#!/usr/bin/env bash
#
# Cloud Agent install script for the NVIDIA GPU Operator.
#
# Prepares a working Go development environment: warms the build cache, installs
# the code-generation tools the Makefile expects under ./bin, and installs the
# pinned golangci-lint and helm binaries used by `make lint` and
# `make validate-helm-values`. The script is idempotent and safe to re-run.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Keep these in sync with versions.mk / docker/Dockerfile.devel.
GOLANGCI_LINT_VERSION="v2.12.2"
# helm is needed by `make validate-helm-values`; pin a recent v3 release.
HELM_VERSION="v3.16.4"

echo "== go build (vendored deps): compile all packages =="
go build ./...

echo "== make install-tools: controller-gen, client-gen, kustomize, gcov2lcov, go-licenses -> ./bin =="
make install-tools

# Install CLI tools that the Makefile invokes from PATH into a PATH directory
# that persists in the environment image/snapshot.
install_to_path() {
  # $1 = friendly name, $2 = module@version to `go install`
  local name="$1" module="$2" tmpbin
  tmpbin="$(mktemp -d)"
  echo "== go install ${name} (${module}) =="
  GOBIN="${tmpbin}" go install "${module}"
  sudo install -m 0755 "${tmpbin}/${name}" "/usr/local/bin/${name}"
  rm -rf "${tmpbin}"
}

install_to_path golangci-lint "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"
install_to_path helm "helm.sh/helm/v3/cmd/helm@${HELM_VERSION}"

echo "== versions =="
go version
golangci-lint version
helm version --short

echo "== install complete =="
