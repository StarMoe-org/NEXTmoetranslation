#!/bin/sh
set -eu

expected_index=9a735e96f856da9b94e1362883df13616a8b6e3cd33afce5d5e1468b4784b475
expected_detail=224a7d34e1d4d551bca21cbe70374f504a781edef90eb644d8d4ec9e5fca064c
expected_db=2eb61967a5f5b96a4961c0258984d6d5bb2f7b813379872d9d50a427704b8877
expected_public_lyrics_bundle=962d1f7931d915f325d703f26b1ce02d30c353b753f9f684163ea1e78d203453
expected_editor_lyrics_seed=a8a2a7c841d0d73e448fd69f9adb236965b3b01a89d2ba58dcc921925e6ea479
expected_public_lyrics_inventory=738e0dcadae6a81d46e000cdde8a080f9e43f8a7ebeb9f8128efab45da23f7a6
expected_public_lyrics_tar=84ad887f9eb4dfdaa1dc699c82beada39487b4a53b311a6022716d06928bbaca
historical_700_public_lyrics_bundle=6a987c5ed796b4609e4bcbc5c67126196eb660258ad19bea672408cb42f9136b
historical_700_public_lyrics_inventory=604aae68e3cd6824a8960a3cbbec5e015af48e5fcdd9895f785ff61e019d1f4b
public_lyrics_bundle=server/internal/publiclyricsbundle/public-v3.tar.gz
editor_lyrics_seed=server/internal/embeddedlyricsseed/editor-seed.tar.gz

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

reject_pattern() {
  pattern=$1
  shift
  if grep -E -q -- "$pattern" "$@"; then
    echo "forbidden release contract pattern found: $pattern" >&2
    exit 1
  fi
}

test "$(hash_file contracts/public-lyrics/v1/index.fixture.json)" = "$expected_index"
test "$(hash_file contracts/public-lyrics/v1/detail.fixture.json)" = "$expected_detail"
test "$(hash_file server/internal/db/testdata/legacy-v2.db)" = "$expected_db"

test -f "$public_lyrics_bundle"
test -f server/internal/publiclyricsbundle/bundle.go
test -f server/internal/publiclyricsbundle/bundle_test.go
test -f scripts/build-public-lyrics-v3-bundle.py
test "$(hash_file "$public_lyrics_bundle")" = "$expected_public_lyrics_bundle"
grep -Fq '//go:embed public-v3.tar.gz' server/internal/publiclyricsbundle/bundle.go
test -f "$editor_lyrics_seed"
test -f server/internal/embeddedlyricsseed/bundle.go
test -f server/internal/embeddedlyricsseed/bundle_test.go
test -f server/internal/embeddedlyricsseed/contract.go
test -f scripts/build-embedded-lyrics-editor-seed.py
test "$(hash_file "$editor_lyrics_seed")" = "$expected_editor_lyrics_seed"
grep -Fq "const ExpectedArchiveSHA256 = \"$expected_editor_lyrics_seed\"" server/internal/embeddedlyricsseed/bundle.go
grep -Fq '//go:embed editor-seed.tar.gz' server/internal/embeddedlyricsseed/bundle.go
grep -Fq 'EXPECTED_DB_SHA256 = "160e9c9c36e066aa6e33c0a09bffb36b08101a9b9e1e6cd99b7b05e13cd9b766"' scripts/build-embedded-lyrics-editor-seed.py
python3 - "$public_lyrics_bundle" "$expected_public_lyrics_inventory" "$expected_public_lyrics_tar" <<'PY'
import collections
import gzip
import hashlib
import io
import json
import re
import sys
import tarfile

bundle, expected_inventory, expected_tar = sys.argv[1:]
raw_tar = gzip.decompress(open(bundle, "rb").read())
if len(raw_tar) != 22568960 or hashlib.sha256(raw_tar).hexdigest() != expected_tar:
    raise SystemExit("public lyrics decompressed tar identity differs")
detail = re.compile(r"music_([1-9][0-9]*)\.json\Z")
with tarfile.open(fileobj=io.BytesIO(raw_tar), mode="r:") as archive:
    members = archive.getmembers()
    bodies = {member.name: archive.extractfile(member).read() for member in members}
if len(members) != 695:
    raise SystemExit(f"public lyrics bundle member count={len(members)}, expected=695")
if sum(member.size for member in members) != 22027427:
    raise SystemExit("public lyrics bundle runtime byte count differs")
names = [member.name for member in members]
if len(names) != len(set(names)) or names.count("index.json") != 1:
    raise SystemExit("public lyrics bundle inventory is duplicate or lacks one index")
if any(
    not member.isfile()
    or member.mode != 0o444
    or member.uid != 0
    or member.gid != 0
    or member.mtime != 0
    or member.uname
    or member.gname
    or member.linkname
    or member.pax_headers
    or member.devmajor != 0
    or member.devminor != 0
    for member in members
):
    raise SystemExit("public lyrics bundle contains noncanonical metadata")
detail_ids = [int(match.group(1)) for name in names if (match := detail.fullmatch(name))]
if len(detail_ids) != 694 or len(detail_ids) != len(set(detail_ids)):
    raise SystemExit("public lyrics bundle detail inventory differs")
if set(names) != {"index.json", *(f"music_{music_id}.json" for music_id in detail_ids)}:
    raise SystemExit("public lyrics bundle contains a nested, private, or unexpected artifact")
inventory = hashlib.sha256()
for name in sorted(bodies):
    body = bodies[name]
    inventory.update(name.encode())
    inventory.update(b"\0")
    inventory.update(hashlib.sha256(body).hexdigest().encode())
    inventory.update(b"\0")
    inventory.update(str(len(body)).encode())
    inventory.update(b"\n")
if inventory.hexdigest() != expected_inventory:
    raise SystemExit("public lyrics bundle computed inventory differs")

def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result

index = json.loads(bodies["index.json"], object_pairs_hook=reject_duplicates)
if index.get("version") != 3:
    raise SystemExit("public lyrics index contract differs")
expected_details = {song["musicId"] for song in index["songs"] if song["state"] in {"complete", "game_only"}}
if expected_details != set(detail_ids):
    raise SystemExit("public lyrics index/detail identity differs")
for music_id in detail_ids:
    document = json.loads(bodies[f"music_{music_id}.json"], object_pairs_hook=reject_duplicates)
    if document.get("version") != 3 or document.get("musicId") != music_id:
        raise SystemExit(f"public lyrics detail identity differs: {music_id}")
for forbidden in (b'"databaseSha256"', b'"manifestSha256"', b'"receiptSha256"', b'"rawBytes"', b'"privateReview"', b'"indexEvidenceRefs"', b'"documentJson"', b'"fixedIdentityJson"', b'"sourceUrl"', b'"sourceSha1"', b'"sourceFetchedAt"', b'"acquisitionId"'):
    if any(forbidden in body for body in bodies.values()):
        raise SystemExit(f"public lyrics bundle contains forbidden private field {forbidden!r}")
PY

if grep -q 'registry\.npmmirror\.com' web/package-lock.json; then
  echo 'package lock contains a noncanonical registry' >&2
  exit 1
fi
if grep -R -q '/sse?token=' web/src; then
  echo 'web console still places a session token in the SSE URL' >&2
  exit 1
fi
grep -q 'Authorization.*Bearer' web/src/lib/fetch-sse.mjs

for image_arg in NODE_IMAGE NODE_IMAGE_DIGEST GO_IMAGE GO_IMAGE_DIGEST RUNTIME_IMAGE RUNTIME_IMAGE_DIGEST; do
  grep -q "ARG $image_arg" Dockerfile
done
grep -Fq 'ARG NODE_IMAGE_DIGEST=df02558528d3d3d0d621f112e232611aecfee7cbc654f6b375765f72bb262799' Dockerfile
grep -Fq 'ARG GO_IMAGE_DIGEST=b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd' Dockerfile
grep -Fq 'ARG RUNTIME_IMAGE=buildpack-deps:bookworm-scm' Dockerfile
grep -Fq 'ARG RUNTIME_IMAGE_DIGEST=877e9e4d949edfbcbedabc3a2d7ab593955fee5d6d0777adf3a991eb30c750d8' Dockerfile
grep -Fq 'FROM ${NODE_IMAGE}@sha256:${NODE_IMAGE_DIGEST}' Dockerfile
grep -Fq 'FROM ${GO_IMAGE}@sha256:${GO_IMAGE_DIGEST}' Dockerfile
grep -Fq 'FROM ${RUNTIME_IMAGE}@sha256:${RUNTIME_IMAGE_DIGEST}' Dockerfile
grep -q '^USER 65532:65532$' Dockerfile
grep -q 'chown -R 0:0 /app' Dockerfile
grep -q 'chmod -R a-w /app' Dockerfile
grep -q '^FROM runtime AS standalone$' Dockerfile
grep -q '^FROM standalone AS next-production$' Dockerfile
grep -q -- '-X main.runtimeProfile=next-production' Dockerfile
grep -q 'COPY --chmod=0555 --from=go-builder /moesekai-server-next-production ./moesekai-server' Dockerfile
grep -q 'MOESEKAI_PRODUCTION=true' Dockerfile
grep -q 'WEB_DIR=/app/web' Dockerfile
grep -q 'DB_PATH=/data/moesekai.db' Dockerfile
grep -q 'DATA_DIR=/data' Dockerfile
grep -q 'TZ=UTC' Dockerfile
grep -q '/usr/share/zoneinfo/UTC' Dockerfile
grep -q 'WORKSPACE_MODE=disabled' Dockerfile
grep -q 'RUN test ! -e /app/workspace && ./moesekai-server --verify-runtime' Dockerfile
reject_pattern 'COPY --from=workspace|--build-context workspace|AS paired|WORKSPACE_WEB_DIR|WORKSPACE_MANIFEST_SHA256' Dockerfile

ci=.github/workflows/ci.yml
release=.github/workflows/release-next.yml
test -f "$ci"
test -f "$release"
test ! -e .github/workflows/release-paired.yml
test ! -e PAIRED_RELEASE.md
test -f STANDALONE_RELEASE.md

grep -q '^  fast:$' "$ci"
grep -q '^  race-api:$' "$ci"
grep -q '^  race-store:$' "$ci"
grep -q '^  race-rest:$' "$ci"
grep -q '^  verify:$' "$ci"
grep -q 'needs: \[fast, race-api, race-store, race-rest\]' "$ci"
grep -q 'timeout-minutes: 45' "$ci"
grep -q 'timeout-minutes: 75' "$ci"
grep -q 'timeout-minutes: 90' "$ci"
grep -q 'timeout-minutes: 60' "$ci"
grep -q 'go test -count=1 -timeout=30m ./...' "$ci"
grep -q 'go test -race -count=1 -timeout=60m ./internal/api' "$ci"
grep -q 'name: Race Store shard' "$ci"
grep -q 'shard: \[0, 1, 2, 3\]' "$ci"
grep -Fq 'while IFS= read -r test_name; do' "$ci"
grep -q 'index % 4 == STORE_SHARD' "$ci"
grep -Fq 'go test -race -count=1 -timeout=45m -run "$regex" ./internal/store' "$ci"
grep -q 'mapfile -t packages' "$ci"
grep -q "grep -Ev '/internal/(api|store)\$'" "$ci"
grep -q 'Build standalone production image' "$ci"
grep -q -- '--target next-production' "$ci"
grep -q 'WORKSPACE_MODE=disabled' "$ci"
grep -q 'WEB_DIR=/app/web' "$ci"
grep -q 'DB_PATH=/data/moesekai.db' "$ci"
grep -q 'DATA_DIR=/data' "$ci"
grep -q 'TZ=UTC' "$ci"
grep -q 'standalone production binary requires TZ to remain exactly' "$ci"
grep -q 'TZ=Pacific/Honolulu' "$ci"
grep -q '/usr/share/zoneinfo/UTC' "$ci"
grep -q 'org.opencontainers.image.source' "$ci"
grep -q 'test ! -e /app/workspace' "$ci"
grep -q "'65532:65532'" "$ci"
grep -q -- '--read-only' "$ci"
grep -q -- '--tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m' "$ci"
grep -q 'Assert standalone production startup fails closed' "$ci"
grep -q 'MOESEKAI_PRODUCTION=false' "$ci"
grep -q 'standalone production binary requires MOESEKAI_PRODUCTION to remain exactly' "$ci"
grep -q 'WORKSPACE_MODE=external' "$ci"
grep -q 'MOESEKAI_MASTER_KEY must contain at least 32 bytes' "$ci"
grep -q 'an initialized administrator is required' "$ci"
grep -q 'standalone production binary requires WEB_DIR to remain exactly' "$ci"
grep -q 'standalone production binary requires DB_PATH to remain exactly' "$ci"
grep -q 'standalone production binary requires DATA_DIR to remain exactly' "$ci"
grep -q 'DATA_DIR=/tmp/data' "$ci"
grep -q 'published ADMIN_PASSWORD template must be replaced' "$ci"
grep -q '%25252Feditor' "$ci"
grep -q '%252Feditor%25' "$ci"
grep -q '%2577orkspace%252Feditor%25' "$ci"
grep -q -- '--path-as-is' "$ci"
grep -q -- '--request OPTIONS' "$ci"
grep -q 'Smoke standalone read-only root and single SQLite writer' "$ci"
grep -q 'database is already owned by another process' "$ci"
grep -q 'test "$ready_status" = 503' "$ci"
grep -q 'for route in /workspace /workspace/ /workspace/editor/cards /workspace/assets/app.js' "$ci"
grep -q 'test "$status" = 404' "$ci"
grep -q "grep -Fx '404 page not found'" "$ci"
grep -q '! cmp -s "$body"' "$ci"
grep -q 'docker stop --time 30' "$ci"
grep -q 'package-rollback.sh' "$ci"
grep -q -- '-X main.runtimeProfile=next-production' "$ci"
grep -q 'cmp dist/nexttrans-rollback.tar' "$ci"
grep -q 'verify-rollback-bundle.py' "$ci"
grep -q 'sha256sum -c SHA256SUMS' "$ci"
grep -q 'Export the exact tested production candidate' "$ci"
grep -q 'docker save --output dist/next-production-candidate/image.tar' "$ci"
grep -q 'name: next-production-candidate-${{ github.sha }}-${{ github.run_attempt }}' "$ci"
grep -q 'retention-days: 90' "$ci"
grep -q 'path: dist/nexttrans-rollback.tar' "$ci"
grep -q 'name: rollback-${{ github.sha }}-${{ github.run_attempt }}' "$ci"
reject_pattern '--target paired|--build-context workspace=|MOE_REVISION|MOE_TAG|moesekai-paired-ci' "$ci"

grep -q '^  workflow_dispatch:$' "$release"
reject_pattern '^  (push|pull_request|schedule|workflow_run):' "$release"
grep -q 'environment: production' "$release"
grep -q 'actions: read' "$release"
grep -q 'packages: none' "$release"
if grep -q 'packages: write' "$release"; then
  echo 'promotion workflow must not grant ordinary GITHUB_TOKEN package-write authority' >&2
  exit 1
fi
grep -q 'attestations: write' "$release"
grep -q 'id-token: write' "$release"
grep -q 'GHCR_RELEASE_USERNAME' "$release"
grep -q 'GHCR_RELEASE_TOKEN' "$release"
grep -q 'push-to-registry: false' "$release"
if grep -q 'push-to-registry: true' "$release"; then
  echo 'optional GitHub attestation must not use GITHUB_TOKEN package-write authority' >&2
  exit 1
fi
grep -Fq 'NEXT_PREDICATE_TYPE: ${{ github.server_url }}/${{ github.repository }}/attestations/next-production-image/v1' "$release"
reject_pattern 'NEXTmoetra[b]slation' "$release" Dockerfile STANDALONE_RELEASE.md
grep -q 'actions/workflows/ci.yml/runs' "$release"
grep -q 'head_sha="$GITHUB_SHA"' "$release"
grep -q 'head_branch == $branch' "$release"
grep -q 'head_repository.full_name == $repository' "$release"
grep -q 'conclusion)" = success' "$release"
grep -q 'test "$DEFAULT_BRANCH" = main' "$release"
test "$(grep -c 'git/ref/heads/main' "$release")" -ge 2
grep -q 'Resolve exact CI candidate and rollback artifacts' "$release"
grep -q 'needs: prepare' "$release"
grep -q 'actions/artifacts/${CANDIDATE_ARTIFACT_ID}/zip' "$release"
grep -q 'actions/artifacts/${ROLLBACK_ARTIFACT_ID}/zip' "$release"
grep -q 'CANDIDATE_ARTIFACT_DIGEST' "$release"
grep -q 'ROLLBACK_ARTIFACT_DIGEST' "$release"
grep -q 'REVIEWED_ROLLBACK_TAR_SHA256' "$release"
grep -q 'verify-rollback-bundle.py' "$release"
grep -q 'TZ=UTC' "$release"
grep -q 'DATA_DIR=/data' "$release"
grep -q 'sha256sum "$archive"' "$release"
grep -q 'docker load --input image.tar' "$release"
grep -q 'docker tag "$LOCAL_IMAGE" "$STAGING_TAG"' "$release"
grep -q 'docker push "$STAGING_TAG"' "$release"
grep -q 'target: "next-production"' "$release"
grep -q "find . -mindepth 1 -maxdepth 1 -printf '%y:%f" "$release"
grep -q 'staging-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}' "$release"
grep -q 'final_tag="${image}:next-${GITHUB_SHA}"' "$release"
grep -q 'cosign sign --yes "$subject"' "$release"
grep -q 'cosign attest --yes' "$release"
grep -q -- '--certificate-github-workflow-sha "$GITHUB_SHA"' "$release"
grep -q 'jq -nS' "$release"
grep -q 'mode: "standalone"' "$release"
grep -q 'baseImages:' "$release"
grep -q '@base64d | fromjson' "$release"
grep -Fq '$statement.predicate == $expected[0]' "$release"
grep -Fq 'any($envelopes[];' "$release"
grep -q 'candidateArtifact:' "$release"
grep -q 'rollbackArtifact:' "$release"
grep -q 'cosign-release: v3.1.2' "$release"
grep -q 'cosign version --json' "$release"
if grep -q 'actions/attest-build-provenance@' "$release"; then
  echo 'promotion workflow must not claim image build provenance' >&2
  exit 1
fi
grep -q 'actions/attest@' "$release"
grep -q "GITHUB_ARTIFACT_ATTESTATIONS_ENABLED == 'true'" "$release"
grep -q 'docker buildx imagetools create --prefer-index=false --tag "$FINAL_TAG"' "$release"
grep -q 'immutable commit tag already points to a different digest' "$release"
reject_pattern 'create-github-app-token|MOE_RELEASE|MOE_TAG|MOE_COMMIT|SnowGlow-aww|SekaiText-Moe' "$release"
reject_pattern 'releases/|releases/assets|resolve-tag|select-assets|validate-downloads|paired-release' "$release"
reject_pattern 'actions/download-artifact|docker/build-push-action|build-args:|context: \.|push: true' "$release"
reject_pattern 'WORKSPACE_WEB_DIR|WORKSPACE_MANIFEST_SHA256|WORKSPACE_ARCHIVE|build-contexts:|workspace=|target: paired|io\.sekaitext' "$release"
reject_pattern '(^|:)latest([[:space:]]|$)|\bdeploy(ment)?\b' "$release"

for workflow in "$ci" "$release"; do
  awk '
    /^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]+/ {
      action=$0
      sub(/^.*uses:[[:space:]]+/, "", action)
      sub(/[[:space:]#].*$/, "", action)
      if (action !~ /^\.\// && action !~ /@[0-9a-f]{40}$/) {
        print "action is not pinned to a full commit SHA: " action > "/dev/stderr"
        exit 1
      }
    }
  ' "$workflow"
done

test -x scripts/package-rollback.sh
test -x scripts/verify-release.sh
test -f scripts/verify-rollback-bundle.py
test -f scripts/verify-rollback-bundle.test.py
python3 -c 'compile(open("scripts/verify-rollback-bundle.py", encoding="utf-8").read(), "scripts/verify-rollback-bundle.py", "exec")'
python3 scripts/verify-rollback-bundle.test.py
grep -q '^# ADMIN_PASSWORD=replace-with-12-or-more-characters$' .env.example
if grep -q '^ADMIN_PASSWORD=replace-with-12-or-more-characters$' .env.example; then
  echo 'published administrator password template is active' >&2
  exit 1
fi
grep -q -- '--verify-runtime' docker-entrypoint.sh
entrypoint_verify_line=$(grep -n -- '--verify-runtime' docker-entrypoint.sh | head -n1 | cut -d: -f1)
entrypoint_mutation_line=$(grep -n -E 'mkdir -p "\$DATA_DIR"|chmod 0700 "\$DATA_DIR"|moesekai-migrate' docker-entrypoint.sh | head -n1 | cut -d: -f1)
test "$entrypoint_verify_line" -lt "$entrypoint_mutation_line"
grep -q 'seed-incomplete' docker-entrypoint.sh
if grep -q 'seed migration failed; starting with empty database' docker-entrypoint.sh; then
  echo 'entrypoint still fails open after seed migration errors' >&2
  exit 1
fi

for document in STANDALONE_RELEASE.md PRODUCTION_CONTRACT.md ROLLBACK_RUNBOOK.md README.md; do
  reject_pattern 'release-paired\.yml|PAIRED_RELEASE\.md' "$document"
done
grep -q 'release-next.yml' STANDALONE_RELEASE.md
grep -q 'WORKSPACE_MODE=disabled' STANDALONE_RELEASE.md
grep -q 'next-<next-full-sha>' STANDALONE_RELEASE.md
grep -qi 'standalone production image' PRODUCTION_CONTRACT.md
grep -q 'lyrics_peer_renditions_and_localizations' PRODUCTION_CONTRACT.md
grep -q 'contracts/public-lyrics/v3/' PRODUCTION_CONTRACT.md
grep -q 'server/internal/publiclyricsbundle/public-v3.tar.gz' PRODUCTION_CONTRACT.md
grep -q 'embedded_lyrics_editor_seed_ledger' PRODUCTION_CONTRACT.md
grep -q 'server/internal/embeddedlyricsseed/editor-seed.tar.gz' STANDALONE_RELEASE.md
grep -q "$expected_public_lyrics_bundle" STANDALONE_RELEASE.md
grep -q "$expected_public_lyrics_bundle" README.md
grep -q "$expected_editor_lyrics_seed" PRODUCTION_CONTRACT.md
grep -q "$expected_editor_lyrics_seed" STANDALONE_RELEASE.md
grep -q "$expected_editor_lyrics_seed" README.md
grep -q 'DB_PATH.*data/moesekai.db' STANDALONE_RELEASE.md
grep -q 'DATA_DIR.*data' STANDALONE_RELEASE.md
grep -q 'TZ.*UTC' STANDALONE_RELEASE.md
grep -q 'commit-addressed' ROLLBACK_RUNBOOK.md
grep -q 'STANDALONE_RELEASE.md' README.md

node --test scripts/release-workflow.test.mjs
