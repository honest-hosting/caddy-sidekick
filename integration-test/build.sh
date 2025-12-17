#!/bin/bash
# Build script for Caddy Sidekick integration test Docker image

set -e

echo "Building caddy-sidekick-integration-test Docker image..."
echo "=================================================="

# Change to the integration-test directory
cd "$(dirname "$0")"

# Build the Docker image using docker-compose
echo "Building with docker-compose..."
docker compose -f docker-compose.build.yml build

echo ""
echo "Build completed successfully!"
echo "Image name: caddy-sidekick-integration-test:latest"
echo ""
echo "To run the integration tests:"
echo "  docker compose -f docker-compose.build.yml up -d"
echo "  go test -v ./..."
echo "  docker compose -f docker-compose.build.yml down"
echo ""
echo "Or simply run: make test-integration"
