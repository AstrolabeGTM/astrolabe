#!/usr/bin/env bash
# Runs the README quickstart for real in an empty directory and checks the
# whole loop: init, compose up, login, start a sequence, approve, outbox,
# simulated reply, sequence stops. Uses ghcr.io/astrolabegtm/astrolabe:latest
# (build and tag it locally first to test unreleased code).
# Usage: scripts/quickstart-test.sh [port]
set -euo pipefail
PORT=${1:-8080}
IMAGE=ghcr.io/astrolabegtm/astrolabe:latest
DIR=$(mktemp -d)
PROJECT=astrolabe-quickstart-$$
cd "$DIR"
fail() { echo "FAIL: $*"; docker compose -p $PROJECT logs astrolabe | tail -40; exit 1; }
cleanup() { docker compose -p $PROJECT down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

# The README commands.
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work $IMAGE init -template demo >/dev/null
[ "$(stat -c %u products 2>/dev/null || stat -f %u products)" = "$(id -u)" ] || fail "init wrote files not owned by you"
sed -i.bak "s/\"8080:8080\"/\"$PORT:8080\"/" compose.yaml
docker compose -p $PROJECT up -d >/dev/null 2>&1 || fail "compose up"

U=http://127.0.0.1:$PORT
for i in $(seq 1 90); do curl -sf $U/healthz >/dev/null && break; sleep 2; done
curl -sf $U/healthz >/dev/null || fail "healthz"
PW=$(grep ^ASTROLABE_PASSWORD .env | cut -d= -f2)
J=$DIR/jar; c() { curl -s -b $J -c $J "$@"; }
[ "$(c -o /dev/null -w '%{http_code}' -d "password=$PW" $U/login)" = 303 ] || fail "login"
c $U/ | grep -q 'Sandbox mode' || fail "sandbox banner"
PEOPLE=$(c "$U/people?product=pipewrench-demo" | grep -o 'name="person" value="[0-9]*"' | wc -l)
[ "$PEOPLE" -ge 5 ] || fail "demo people not seeded ($PEOPLE)"
c -o /dev/null -d product=pipewrench-demo -d sequence=intro -d person=1 $U/people/start
c $U/ -o inbox.html
ID=$(grep -o 'actions/[0-9]*/save' inbox.html | head -1 | grep -o '[0-9]*') || fail "no draft"
SUBJ=$(grep -o 'name="subject" value="[^"]*"' inbox.html | head -1 | sed 's/.*value="//;s/"$//')
BODY=$(python3 -c "import re,html;s=open('inbox.html').read();m=re.search(r'<textarea name=\"body\"[^>]*>(.*?)</textarea>',s,re.S);print(html.unescape(m.group(1)))")
c -o /dev/null --data-urlencode "subject=$SUBJ" --data-urlencode "body=$BODY" -d then=approve $U/actions/$ID/save
for i in $(seq 1 30); do c $U/outbox -o outbox.html; grep -q 'outbox/[0-9]*/reply' outbox.html && break; sleep 2; done
OB=$(grep -o 'outbox/[0-9]*/reply' outbox.html | head -1 | grep -o '[0-9]*') || fail "nothing in the outbox"
c -o /dev/null -d "body=Yes please" $U/outbox/$OB/reply
c "$U/people?product=pipewrench-demo" | grep -q 'stopped: replied' || fail "reply did not stop the sequence"
# The server can write where it needs to.
docker compose -p $PROJECT exec -T astrolabe sh -c 'touch /data/products/.w /data/secrets/.w' || fail "data dirs not writable"
! docker compose -p $PROJECT logs astrolabe 2>&1 | grep -q 'level=ERROR' || fail "errors in the log"
echo "quickstart ok"
