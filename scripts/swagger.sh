#!/usr/bin/env bash
set -euo pipefail

# Resolve the project root relative to this script's location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${PROJECT_ROOT}"

# Install swag CLI if not present.
if ! command -v swag &> /dev/null; then
    echo "Installing swag..."
    go install github.com/swaggo/swag/cmd/swag@latest
fi

# Generate swagger docs.
echo "Generating swagger docs..."
swag init \
    -g internal/api/doc.go \
    -o docs \
    --parseDependency \
    --parseInternal

echo "Done. Swagger docs generated in docs/"
