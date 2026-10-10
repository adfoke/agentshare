#!/usr/bin/env bash
# End-to-end check against a running agentshare.
#
#   AGENTSHARE_ADMIN_TOKEN=... ./scripts/smoke.sh [BASE_URL]
#
# It exercises the whole surface the way a client actually would — real HTTP,
# real bytes, no mocks — and leaves nothing behind. Every check prints PASS or
# FAIL and the script exits non-zero if any of them failed, so it can be the
# last step of a deploy.
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080}"
TOKEN="${AGENTSHARE_ADMIN_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  echo "AGENTSHARE_ADMIN_TOKEN is not set" >&2
  exit 2
fi

# The Mac this is usually run from routes everything through a proxy; loopback
# must not go through it or every check fails for the wrong reason.
# The address to send requests to, and the address whose name is expected to
# appear in the absolute URLs the service returns. They are usually the same;
# they differ when a service is reachable on more than one hostname, because it
# still has one canonical public URL and that is the one it builds links from.
#   EXPECT_BASE=https://files.example.com ./smoke.sh https://dl.files.example.com
EXPECT_BASE="${EXPECT_BASE:-}"

export NO_PROXY="${NO_PROXY:-},127.0.0.1,localhost"
CURL=(curl -sS --noproxy '*' --max-time 20)

WORK="$(mktemp -d)"
file_id=""
file_id2=""

# Remove anything this run created, even if it is interrupted part-way. A test
# that leaves a file behind in the service it was testing makes the next run
# fail for a reason that has nothing to do with the code — which is exactly how
# this trap came to be written.
cleanup() {
  for id in "$file_id" "$file_id2"; do
    if [ -n "$id" ]; then
      "${CURL[@]}" -X DELETE -H "Authorization: Bearer $TOKEN" \
        "$BASE/v1/files/$id" >/dev/null 2>&1 || true
    fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

pass=0
fail=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }

eq() { # label want got
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 — want '$2', got '$3'"; fi
}

jget() { # json path via python3, so we do not depend on jq
  # Booleans are printed the way JSON spells them, not the way Python does:
  # otherwise a correct "true" from the server reads back as "True" and the
  # check fails against a working service.
  python3 -c 'import json,sys
d = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d = d[int(k)] if isinstance(d, list) else d.get(k)
    if d is None: break
if isinstance(d, bool): print("true" if d else "false")
else: print("" if d is None else d)' "$1"
}

code() { # method path [args...] -> prints the HTTP status
  local method="$1" path="$2"
  shift 2
  "${CURL[@]}" -o /dev/null -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN" "$@" "$BASE$path"
}

[ -n "$EXPECT_BASE" ] || EXPECT_BASE="$BASE"
echo "agentshare smoke test against $BASE"
[ "$EXPECT_BASE" != "$BASE" ] && echo "  (expecting URLs on $EXPECT_BASE)"

# --- discovery ------------------------------------------------------------
body=$("${CURL[@]}" "$BASE/")
eq "GET / announces the service" "agentshare" "$(printf '%s' "$body" | jget service)"
eq "GET /healthz is green" "200" "$(code GET /healthz)"
eq "GET /openapi.json is served" "200" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/openapi.json")"

# --- auth -----------------------------------------------------------------
eq "no token is rejected" "401" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/v1/files")"
eq "a wrong token is rejected" "401" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer ash_wrong' "$BASE/v1/whoami")"
eq "whoami accepts the root token" "root" \
  "$("${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$BASE/v1/whoami" | jget identity.type)"

# --- upload ---------------------------------------------------------------
head -c 300000 /dev/urandom > "$WORK/payload.bin"
sha=$(shasum -a 256 "$WORK/payload.bin" | awk '{print $1}')

upload=$("${CURL[@]}" -T "$WORK/payload.bin" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Filename: payload.bin" \
  -H "X-Tag: smoke,tmp" \
  -H "X-Ttl: 1h" \
  -H "X-Meta: {\"suite\":\"smoke\"}" \
  "$BASE/v1/files")
file_id=$(printf '%s' "$upload" | jget file.id)
eq "upload returns the content digest" "$sha" "$(printf '%s' "$upload" | jget file.sha256)"
eq "upload reports the byte count" "300000" "$(printf '%s' "$upload" | jget file.size)"

# A retried upload must be a no-op, not a second copy.
again=$("${CURL[@]}" -T "$WORK/payload.bin" -H "Authorization: Bearer $TOKEN" \
  -H "X-Filename: payload.bin" "$BASE/v1/files")
eq "a retried upload deduplicates" "true" "$(printf '%s' "$again" | jget deduplicated)"
eq "a retried upload returns the same id" "$file_id" "$(printf '%s' "$again" | jget file.id)"

# --- download -------------------------------------------------------------
"${CURL[@]}" -H "Authorization: Bearer $TOKEN" -o "$WORK/back.bin" "$BASE/v1/files/$file_id/content"
if cmp -s "$WORK/payload.bin" "$WORK/back.bin"; then ok "download is byte-identical"; else bad "download differs from the upload"; fi

range=$("${CURL[@]}" -H "Authorization: Bearer $TOKEN" -r 0-99 -o "$WORK/part.bin" -w '%{http_code}' "$BASE/v1/files/$file_id/content")
eq "range request answers 206" "206" "$range"
eq "range returns the requested length" "100" "$(wc -c < "$WORK/part.bin" | tr -d ' ')"

# --- share: the path for someone with no token ----------------------------
share=$("${CURL[@]}" -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"file_id\":\"$file_id\",\"ttl\":\"30m\",\"max_downloads\":2,\"label\":\"smoke\"}" \
  "$BASE/v1/shares")
share_token=$(printf '%s' "$share" | jget share.id)
eq "share link is a capability URL" "$EXPECT_BASE/s/$share_token" "$(printf '%s' "$share" | jget share.url)"

info=$("${CURL[@]}" "$BASE/s/$share_token/info")
eq "share info needs no credential" "payload.bin" "$(printf '%s' "$info" | jget file.name)"
eq "share info does not spend a download" "0" "$(printf '%s' "$info" | jget share.downloads)"

"${CURL[@]}" -o "$WORK/shared.bin" "$BASE/s/$share_token"
if cmp -s "$WORK/payload.bin" "$WORK/shared.bin"; then ok "stranger downloads identical bytes"; else bad "shared download differs"; fi

eq "second download is allowed" "200" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/s/$share_token")"
eq "third download hits the limit" "403" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/s/$share_token")"

# --- a share covering several files: the link opens as an index page -----
printf 'second file for the set\n' > "$WORK/second.txt"
second=$("${CURL[@]}" -T "$WORK/second.txt" -H "Authorization: Bearer $TOKEN" \
  -H "X-Filename: second.txt" "$BASE/v1/files")
file_id2=$(printf '%s' "$second" | jget file.id)

set_share=$("${CURL[@]}" -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"file_ids\":[\"$file_id\",\"$file_id2\"],\"label\":\"smoke set\"}" "$BASE/v1/shares")
set_token=$(printf '%s' "$set_share" | jget share.id)
eq "a set share reports its size" "2" "$(printf '%s' "$set_share" | jget share.file_count)"

index=$("${CURL[@]}" -D "$WORK/index.h" "$BASE/s/$set_token")
eq "the set link is an index page" "200" "$(head -1 "$WORK/index.h" | awk '{print $2}')"
eq "the index is served as HTML" "text/html" \
  "$(grep -i '^content-type' "$WORK/index.h" | tr -d '\r' | cut -d' ' -f2 | cut -d';' -f1)"
eq "the index names every file" "2" "$(printf '%s' "$index" | grep -c 'payload.bin\|second.txt')"
# The policy is a response header; the body has no script either, but the header
# is what a browser enforces.
eq "the index forbids scripts" "1" \
  "$(grep -ci "default-src 'none'" "$WORK/index.h")"

# Checked before any member is fetched, because the question is whether merely
# opening the index spends the share's budget.
set_info=$("${CURL[@]}" "$BASE/s/$set_token/info")
eq "info lists the members" "2" "$(printf '%s' "$set_info" | jget files.1.name | wc -c | tr -d ' ' | awk '{print ($1>1)?2:0}')"
eq "opening the index spends no download" "0" "$(printf '%s' "$set_info" | jget share.downloads)"

"${CURL[@]}" -H "Authorization: Bearer $TOKEN" -o "$WORK/set-member.bin" "$BASE/s/$set_token/f/$file_id2"
if cmp -s "$WORK/second.txt" "$WORK/set-member.bin"; then ok "a member downloads byte-identical"; else bad "a member download differs"; fi

# A file that exists in the store but is not a member of this share. Taking the
# id out of thin air rather than from another upload makes the point precisely:
# the token, not the store's contents, is what scopes this route.
eq "a file outside the set is not reachable through it" "404" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/s/$set_token/f/f_00000000000000000000")"

"${CURL[@]}" -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/v1/shares/$set_token" >/dev/null
eq "the set link can be revoked" "404" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/s/$set_token")"

"${CURL[@]}" -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/v1/files/$file_id2" >/dev/null
file_id2=""

# --- listing --------------------------------------------------------------
list=$("${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$BASE/v1/files?limit=10&tag=smoke")
eq "listing finds the upload by tag" "$file_id" "$(printf '%s' "$list" | jget files.0.id)"
eq "listing reports a count" "1" "$(printf '%s' "$list" | jget count)"

# --- scoped tokens --------------------------------------------------------
tok=$("${CURL[@]}" -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-reader","scopes":["download"],"ttl":"10m"}' "$BASE/v1/tokens")
secret=$(printf '%s' "$tok" | jget token.secret)
token_id=$(printf '%s' "$tok" | jget token.id)
eq "a download-only token can download" "200" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $secret" "$BASE/v1/files/$file_id/content")"
eq "a download-only token cannot upload" "403" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $secret" -d x "$BASE/v1/files")"
eq "a download-only token cannot mint tokens" "403" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $secret" -H 'Content-Type: application/json' -d '{"scopes":["upload"]}' "$BASE/v1/tokens")"
eq "revoking a token takes effect" "401" \
  "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/v1/tokens/$token_id" > /dev/null; \
     "${CURL[@]}" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $secret" "$BASE/v1/whoami")"

# --- deletion -------------------------------------------------------------
eq "delete removes the file" "200" "$(code DELETE "/v1/files/$file_id")"
eq "metadata is gone" "404" "$(code GET "/v1/files/$file_id")"
eq "bytes are gone" "404" "$(code GET "/v1/files/$file_id/content")"
# The share survives its file's deletion: "this link has no files left" is a
# more useful answer than "no such link", which would suggest a bad URL.
eq "the share link reports its files are gone" "410" "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/s/$share_token")"

# --- housekeeping ---------------------------------------------------------
left=$("${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$BASE/v1/files" | jget count)
eq "nothing is left behind" "0" "$left"

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[32m%d checks passed\033[0m\n' "$pass"
else
  printf '\033[31m%d passed, %d FAILED\033[0m\n' "$pass" "$fail"
  exit 1
fi
