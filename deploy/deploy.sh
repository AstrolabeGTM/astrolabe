#!/bin/bash
# Ship your products folder to the VM and (re)start the published image.
# Usage: deploy/deploy.sh <project> <zone> <workspace dir containing products/>
set -euo pipefail
project=$1 zone=$2 workspace=$3
[ -d "$workspace/products" ] || { echo "$workspace/products not found"; exit 1; }
tmp=$(mktemp -d)
tar -C "$workspace" -czf "$tmp/products.tgz" products
gcloud compute scp --project "$project" --zone "$zone" --tunnel-through-iap "$tmp/products.tgz" astrolabe:/tmp/
gcloud compute ssh astrolabe --project "$project" --zone "$zone" --tunnel-through-iap --command '
  set -e
  sudo rm -rf /opt/astrolabe/products && sudo tar -C /opt/astrolabe -xzf /tmp/products.tgz
  sudo chown -R 10001 /opt/astrolabe/products
  sudo /opt/astrolabe/start.sh'
rm -rf "$tmp"
