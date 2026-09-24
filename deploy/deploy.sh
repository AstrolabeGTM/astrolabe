#!/bin/bash
# Build for the VM, copy the binary and product folders, restart.
# Usage: deploy/deploy.sh <project> <zone>
set -euo pipefail
project=$1 zone=$2
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -C "$root" -o "$tmp/astrolabe" ./cmd/astrolabe
tar -C "$root" -czf "$tmp/products.tgz" products
gcloud compute scp --project "$project" --zone "$zone" --tunnel-through-iap "$tmp/astrolabe" "$tmp/products.tgz" astrolabe:/tmp/
gcloud compute ssh astrolabe --project "$project" --zone "$zone" --tunnel-through-iap --command '
  set -e
  sudo install -m 0755 -o astrolabe /tmp/astrolabe /opt/astrolabe/astrolabe
  sudo rm -rf /opt/astrolabe/products && sudo tar -C /opt/astrolabe -xzf /tmp/products.tgz && sudo chown -R astrolabe /opt/astrolabe/products
  sudo systemctl restart astrolabe
  sleep 3 && systemctl is-active astrolabe'
rm -rf "$tmp"
