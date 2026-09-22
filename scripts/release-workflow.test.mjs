import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import test from 'node:test'
import { gunzipSync } from 'node:zlib'

const root = new URL('../', import.meta.url)
const workflow = readFileSync(new URL('.github/workflows/release-next.yml', root), 'utf8')
const ciWorkflow = readFileSync(new URL('.github/workflows/ci.yml', root), 'utf8')
const dockerfile = readFileSync(new URL('Dockerfile', root), 'utf8')
const entrypoint = readFileSync(new URL('docker-entrypoint.sh', root), 'utf8')
const releaseDocumentation = readFileSync(new URL('STANDALONE_RELEASE.md', root), 'utf8')
const productionContract = readFileSync(new URL('PRODUCTION_CONTRACT.md', root), 'utf8')
const rollbackRunbook = readFileSync(new URL('ROLLBACK_RUNBOOK.md', root), 'utf8')
const readme = readFileSync(new URL('README.md', root), 'utf8')
const publicLyricsBundlePath = new URL('server/internal/publiclyricsbundle/public-v3.tar.gz', root)
const publicLyricsBundleSource = readFileSync(new URL('server/internal/publiclyricsbundle/bundle.go', root), 'utf8')
const publicLyricsBundleBuilder = readFileSync(new URL('scripts/build-public-lyrics-v3-bundle.py', root), 'utf8')
const publicLyricsBundle = readFileSync(publicLyricsBundlePath)
const expectedPublicLyricsBundleSHA256 = '962d1f7931d915f325d703f26b1ce02d30c353b753f9f684163ea1e78d203453'

function stepSection(source, name, nextName) {
  const start = source.indexOf(`      - name: ${name}\n`)
  const end = nextName ? source.indexOf(`      - name: ${nextName}\n`, start + 1) : source.length
  assert.ok(start >= 0 && end > start, `missing ${name} step`)
  return source.slice(start, end)
}

function assertActionsPinned(source, label) {
  const uses = [...source.matchAll(/^\s*(?:-\s+)?uses:\s+([^\s#]+)/gm)]
  assert.ok(uses.length > 0, `${label} has no external actions`)
  for (const match of uses) {
    if (match[1].startsWith('./')) continue
    assert.match(match[1], /@[0-9a-f]{40}$/, `${label}: ${match[1]} is not SHA-pinned`)
  }
}

// The bundle is a canonical GNU tar: regular files only, no long names, no
// prefix field in use, so the 512-byte headers can be walked directly.
function tarMembers(archive) {
  const members = []
  for (let offset = 0; offset + 512 <= archive.length; ) {
    const header = archive.subarray(offset, offset + 512)
    const name = header.subarray(0, 100).toString('utf8').replace(/\0.*$/s, '')
    if (name === '') break
    const size = Number.parseInt(header.subarray(124, 136).toString('ascii').replace(/\0.*$/s, '').trim(), 8)
    members.push({ name, body: archive.subarray(offset + 512, offset + 512 + size) })
    offset += 512 + Math.ceil(size / 512) * 512
  }
  return members
}

test('paired release workflow and runbook are retired by deletion', () => {
  assert.equal(existsSync(new URL('.github/workflows/release-paired.yml', root)), false)
  assert.equal(existsSync(new URL('PAIRED_RELEASE.md', root)), false)
  assert.equal(existsSync(new URL('.github/workflows/release-next.yml', root)), true)
  assert.equal(existsSync(new URL('STANDALONE_RELEASE.md', root)), true)
})

test('the accepted public lyrics bundle is present and content-addressed', () => {
  assert.equal(existsSync(publicLyricsBundlePath), true)
  assert.equal(createHash('sha256').update(publicLyricsBundle).digest('hex'), expectedPublicLyricsBundleSHA256)
  assert.match(publicLyricsBundleSource, /validateDocuments/)
  assert.match(publicLyricsBundleSource, /DecodePublicLyricsV3Index/)
  assert.match(publicLyricsBundleSource, /DecodePublicLyricsV3Detail/)
  assert.match(publicLyricsBundleSource, /go:embed public-v3\.tar\.gz/)
  assert.match(publicLyricsBundleBuilder, /EXPECTED_MANIFEST_SHA256 = \"b88f3076e40a6711b9e6a55321ede9da0aef0b69489a22b5b74fe468f5676d6f\"/)
  assert.match(publicLyricsBundleBuilder, /EXPECTED_RECEIPT_FILE_SHA256 = \"a4bf207f446feffd71f2e51ab1755ac3c9cd648b34fe72596f85de3c6a559deb\"/)
})

test('the public lyrics bundle builder pins the counts the accepted bundle actually has', () => {
  const members = tarMembers(gunzipSync(publicLyricsBundle))
  const index = members.find(member => member.name === 'index.json')
  assert.ok(index, 'bundle has no index.json')
  const catalog = JSON.parse(index.body.toString('utf8')).songs.length
  const details = members.filter(member => /^music_[1-9][0-9]*\.json$/.test(member.name)).length
  assert.equal(details + 1, members.length)
  assert.match(publicLyricsBundleBuilder, new RegExp(`^EXPECTED_CATALOG = ${catalog}$`, 'm'))
  assert.match(publicLyricsBundleBuilder, new RegExp(`^EXPECTED_DETAILS = ${details}$`, 'm'))
  assert.match(publicLyricsBundleBuilder, new RegExp(`^EXPECTED_ASSETS = ${members.length}$`, 'm'))
})

test('Docker defaults to a standalone production target without workspace bytes', () => {
  assert.match(dockerfile, /FROM runtime AS standalone/)
  assert.match(dockerfile, /FROM standalone AS next-production\s*$/)
  assert.match(dockerfile, /-X main\.runtimeProfile=next-production/)
  assert.match(dockerfile, /COPY --chmod=0555 --from=go-builder \/moesekai-server-next-production \.\/moesekai-server/)
  assert.match(dockerfile, /MOESEKAI_PRODUCTION=true/)
  assert.match(dockerfile, /WEB_DIR=\/app\/web/)
  assert.match(dockerfile, /DB_PATH=\/data\/moesekai\.db/)
  assert.match(dockerfile, /DATA_DIR=\/data/)
  assert.match(dockerfile, /TZ=UTC/)
  assert.match(dockerfile, /\/usr\/share\/zoneinfo\/UTC/)
  assert.match(dockerfile, /WORKSPACE_MODE=disabled/)
  assert.match(dockerfile, /RUN test ! -e \/app\/workspace && \.\/moesekai-server --verify-runtime/)
  assert.doesNotMatch(dockerfile, /COPY --from=workspace|--build-context workspace|AS paired|WORKSPACE_MANIFEST_SHA256|WORKSPACE_WEB_DIR/)
  for (const name of ['NODE_IMAGE', 'NODE_IMAGE_DIGEST', 'GO_IMAGE', 'GO_IMAGE_DIGEST', 'RUNTIME_IMAGE', 'RUNTIME_IMAGE_DIGEST']) {
    assert.match(dockerfile, new RegExp(`ARG ${name}`))
  }
  assert.match(dockerfile, /ARG NODE_IMAGE_DIGEST=df02558528d3d3d0d621f112e232611aecfee7cbc654f6b375765f72bb262799/)
  assert.match(dockerfile, /ARG GO_IMAGE_DIGEST=b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd/)
  assert.match(dockerfile, /ARG RUNTIME_IMAGE=buildpack-deps:bookworm-scm/)
  assert.match(dockerfile, /ARG RUNTIME_IMAGE_DIGEST=877e9e4d949edfbcbedabc3a2d7ab593955fee5d6d0777adf3a991eb30c750d8/)
  assert.match(dockerfile, /FROM \$\{NODE_IMAGE\}@sha256:\$\{NODE_IMAGE_DIGEST\}/)
  assert.match(dockerfile, /FROM \$\{GO_IMAGE\}@sha256:\$\{GO_IMAGE_DIGEST\}/)
  assert.match(dockerfile, /FROM \$\{RUNTIME_IMAGE\}@sha256:\$\{RUNTIME_IMAGE_DIGEST\}/)
})

test('entrypoint verifies runtime policy before persistent-data mutation', () => {
  const verifyIndex = entrypoint.indexOf('./moesekai-server --verify-runtime')
  assert.ok(verifyIndex >= 0)
  const mutationIndexes = [
    entrypoint.indexOf('mkdir -p "$DATA_DIR"'),
    entrypoint.indexOf('chmod 0700 "$DATA_DIR"'),
    entrypoint.indexOf('./moesekai-migrate'),
  ].filter(index => index >= 0)
  assert.ok(mutationIndexes.length > 0)
  assert.ok(mutationIndexes.every(index => verifyIndex < index))
})

test('CI builds and characterizes only the standalone production image', () => {
  assert.match(ciWorkflow, /^  fast:/m)
  assert.match(ciWorkflow, /^  race-api:/m)
  assert.match(ciWorkflow, /^  race-store:/m)
  assert.match(ciWorkflow, /^  race-rest:/m)
  assert.match(ciWorkflow, /^  verify:/m)
  assert.match(ciWorkflow, /needs: \[fast, race-api, race-store, race-rest\]/)
  assert.match(ciWorkflow, /go test -count=1 -timeout=30m \.\/\.\.\./)
  assert.match(ciWorkflow, /go test -race -count=1 -timeout=60m \.\/internal\/api/)
  assert.match(ciWorkflow, /name: Race Store shard \$\{\{ matrix\.shard \}\}/)
  assert.match(ciWorkflow, /shard: \[0, 1, 2, 3\]/)
  assert.match(ciWorkflow, /while IFS= read -r test_name; do/)
  assert.match(ciWorkflow, /index % 4 == STORE_SHARD/)
  assert.match(ciWorkflow, /go test -race -count=1 -timeout=45m -run "\$regex" \.\/internal\/store/)
  assert.match(ciWorkflow, /mapfile -t packages/)
  assert.match(ciWorkflow, /grep -Ev '\/internal\/\(api\|store\)\$'/)
  assert.match(ciWorkflow, /go test -race -count=1 -timeout=60m "\$\{packages\[@\]\}"/)
  assert.match(ciWorkflow, /name: Build standalone production image/)
  assert.match(ciWorkflow, /--target next-production/)
  assert.match(ciWorkflow, /CI_IMAGE: nexttrans-standalone-ci:/)
  assert.doesNotMatch(ciWorkflow, /--target paired|--build-context workspace=|MOE_REVISION|MOE_TAG|moesekai-paired-ci/)
  assert.match(ciWorkflow, /WORKSPACE_MODE=disabled/)
  assert.match(ciWorkflow, /WEB_DIR=\/app\/web/)
  assert.match(ciWorkflow, /DB_PATH=\/data\/moesekai\.db/)
  assert.match(ciWorkflow, /DATA_DIR=\/data/)
  assert.match(ciWorkflow, /TZ=UTC/)
  assert.match(ciWorkflow, /standalone production binary requires TZ to remain exactly/)
  assert.match(ciWorkflow, /TZ=Pacific\/Honolulu/)
  assert.match(ciWorkflow, /\/usr\/share\/zoneinfo\/UTC/)
  assert.match(ciWorkflow, /test ! -e \/app\/workspace/)
  assert.match(ciWorkflow, /org\.opencontainers\.image\.source/)
  assert.match(ciWorkflow, /'65532:65532'/)
  assert.match(ciWorkflow, /--read-only/)
  assert.match(ciWorkflow, /--tmpfs \/tmp:rw,noexec,nosuid,nodev,size=64m/)
})

test('CI exports the exact tested image and immutable rollback inputs', () => {
  const candidate = stepSection(ciWorkflow, 'Export the exact tested production candidate', 'Upload exact tested production candidate')
  assert.match(candidate, /docker save --output dist\/next-production-candidate\/image\.tar "\$CI_IMAGE"/)
  assert.match(candidate, /target: "next-production"/)
  assert.match(candidate, /baseImages:/)
  assert.match(candidate, /sha256sum image\.tar metadata\.json > SHA256SUMS/)
  assert.match(ciWorkflow, /name: next-production-candidate-\$\{\{ github\.sha \}\}-\$\{\{ github\.run_attempt \}\}/)
  assert.match(ciWorkflow, /name: rollback-\$\{\{ github\.sha \}\}-\$\{\{ github\.run_attempt \}\}/)
  assert.equal((ciWorkflow.match(/retention-days: 90/g) || []).length, 2)
})

test('CI proves workspace routes are disabled and cannot fall back to the console', () => {
  const smoke = stepSection(ciWorkflow, 'Smoke standalone read-only root and single SQLite writer', 'Export the exact tested production candidate')
  assert.doesNotMatch(smoke, /CONSOLE_ORIGIN=/)
  for (const path of ['/workspace', '/workspace/', '/workspace/editor/cards', '/workspace/assets/app.js']) {
    assert.ok(smoke.includes(path), `missing ${path} probe`)
  }
  assert.match(smoke, /test "\$status" = 404/)
  assert.match(smoke, /grep -Fx '404 page not found'/)
  assert.match(smoke, /! cmp -s "\$body" "\$RUNNER_TEMP\/nexttrans-console\.html"/)
  assert.match(smoke, /--path-as-is/)
  assert.match(smoke, /%2e%2e\/workspace/)
  assert.match(smoke, /%25252Feditor/)
  assert.match(smoke, /%252Feditor%25/)
  assert.match(smoke, /%2577orkspace%252Feditor%25/)
  assert.match(smoke, /%5Ceditor/)
  assert.match(smoke, /--request OPTIONS/)
  assert.match(smoke, /! grep -Fqi '<!doctype html'/)
})

test('CI proves fail-closed startup and one SQLite writer', () => {
  const gate = stepSection(ciWorkflow, 'Assert standalone production startup fails closed', 'Smoke standalone read-only root and single SQLite writer')
  assert.match(gate, /-e MOESEKAI_PRODUCTION=false/)
  assert.match(gate, /standalone production binary requires MOESEKAI_PRODUCTION to remain exactly/)
  assert.match(gate, /-e WORKSPACE_MODE=external/)
  assert.match(gate, /-e WORKSPACE_WEB_DIR=/)
  assert.match(gate, /-e WORKSPACE_MANIFEST_SHA256=/)
  assert.match(gate, /-e WEB_DIR=\/data/)
  assert.match(gate, /standalone production binary requires WEB_DIR to remain exactly/)
  assert.match(gate, /DB_PATH=\/data\/moesekai\.db\?mode=rwc/)
  assert.match(gate, /standalone production binary requires DB_PATH to remain exactly/)
  assert.match(gate, /DATA_DIR=\/tmp\/data/)
  assert.match(gate, /standalone production binary requires DATA_DIR to remain exactly/)
  assert.match(gate, /published ADMIN_PASSWORD template must be replaced/)
  assert.match(gate, /must be exactly "disabled" in production/)
  assert.match(gate, /assert_preflight_data_untouched/)
  assert.match(gate, /persistent-volume-sentinel/)
  assert.match(gate, /test ! -e "\$preflight_data\/moesekai\.db"/)
  assert.match(gate, /MOESEKAI_MASTER_KEY must contain at least 32 bytes/)
  assert.match(gate, /an initialized administrator is required/)
  assert.match(ciWorkflow, /database is already owned by another process/)
  assert.match(ciWorkflow, /test "\$ready_status" = 503/)
  assert.match(ciWorkflow, /docker stop --time 30/)
  assert.match(ciWorkflow, /shutdown requested by terminated/)
})

test('standalone publication is manual protected and least privilege', () => {
  assert.match(workflow, /workflow_dispatch:/)
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule|workflow_run):/m)
  assert.match(workflow, /^  prepare:/m)
  assert.match(workflow, /^  publish:/m)
  assert.match(workflow, /needs: prepare/)
  assert.equal((workflow.match(/environment: production/g) || []).length, 1)
  assert.ok(workflow.indexOf('Record exact inputs awaiting production approval') < workflow.indexOf('environment: production'))
  assert.match(workflow, /group: next-release-\$\{\{ github\.repository \}\}/)
  assert.match(workflow, /test "\$DEFAULT_BRANCH" = main/)
  assert.match(workflow, /test "\$GITHUB_REF" = 'refs\/heads\/main'/)
  assert.ok((workflow.match(/git\/ref\/heads\/main/g) || []).length >= 4)
  assert.match(workflow, /ref: \$\{\{ github\.sha \}\}/)
  assert.match(workflow, /persist-credentials: false/)
  assert.match(workflow, /contents: read/)
  assert.match(workflow, /actions: read/)
  assert.match(workflow, /packages: none/)
  assert.doesNotMatch(workflow, /packages: write/)
  assert.match(workflow, /attestations: write/)
  assert.match(workflow, /id-token: write/)
  assert.match(workflow, /NEXT_PREDICATE_TYPE: \$\{\{ github\.server_url \}\}\/\$\{\{ github\.repository \}\}\/attestations\/next-production-image\/v1/)
  assert.doesNotMatch(workflow, /contents: write/)
  assert.doesNotMatch(workflow + dockerfile + releaseDocumentation, /NEXTmoetra[b]slation/)
})

test('release requires successful push CI for the exact default-branch commit', () => {
  const gate = stepSection(workflow, 'Require successful push CI for the exact NEXT commit', 'Resolve exact CI candidate and rollback artifacts')
  assert.match(gate, /actions\/workflows\/ci\.yml\/runs/)
  assert.match(gate, /head_sha="\$GITHUB_SHA"/)
  assert.match(gate, /event=push/)
  assert.match(gate, /\.head_sha == \$sha/)
  assert.match(gate, /\.head_branch == \$branch/)
  assert.match(gate, /\.event == "push"/)
  assert.match(gate, /\.head_repository\.full_name == \$repository/)
  assert.match(gate, /sort_by\(\.run_number, \.run_attempt\)/)
  assert.match(gate, /last \/\/ empty/)
  assert.match(gate, /\.status\)" = completed/)
  assert.match(gate, /\.conclusion\)" = success/)
  assert.doesNotMatch(gate, /success_count|test "\$success_count" -ge 1/)
  assert.ok(workflow.indexOf('Require successful push CI for the exact NEXT commit') < workflow.indexOf('Push the exact tested CI candidate to the staging tag'))
})

test('release has no producer token tag release asset or workspace dependency', () => {
  assert.doesNotMatch(workflow, /create-github-app-token|MOE_RELEASE|MOE_TAG|MOE_COMMIT|SnowGlow-aww|SekaiText-Moe/)
  assert.doesNotMatch(workflow, /releases\/|releases\/assets|resolve-tag|select-assets|validate-downloads|paired-release/)
  assert.doesNotMatch(workflow, /WORKSPACE_WEB_DIR|WORKSPACE_MANIFEST_SHA256|WORKSPACE_ARCHIVE|build-contexts:|workspace=/)
  assert.doesNotMatch(workflow, /target: paired|io\.sekaitext|pair\./)
})

test('release promotes the exact CI artifact without rebuilding it', () => {
  assert.match(workflow, /needs: prepare/)
  assert.match(workflow, /username: \$\{\{ vars\.GHCR_RELEASE_USERNAME \}\}/)
  assert.match(workflow, /password: \$\{\{ secrets\.GHCR_RELEASE_TOKEN \}\}/)
  assert.doesNotMatch(workflow, /password: \$\{\{ (?:github\.token|secrets\.GITHUB_TOKEN) \}\}/)
  assert.match(workflow, /Resolve exact CI candidate and rollback artifacts/)
  assert.match(workflow, /actions\/artifacts\/\$\{CANDIDATE_ARTIFACT_ID\}\/zip/)
  assert.match(workflow, /actions\/artifacts\/\$\{ROLLBACK_ARTIFACT_ID\}\/zip/)
  assert.match(workflow, /CANDIDATE_ARTIFACT_DIGEST/)
  assert.match(workflow, /ROLLBACK_ARTIFACT_DIGEST/)
  assert.match(workflow, /REVIEWED_ROLLBACK_TAR_SHA256/)
  assert.match(workflow, /verify-rollback-bundle\.py/)
  assert.match(workflow, /DATA_DIR=\/data/)
  assert.match(workflow, /TZ=UTC/)
  assert.match(workflow, /sha256sum "\$archive"/)
  assert.match(workflow, /test "\$\(find \. -mindepth 1 -maxdepth 1 -printf '%y:%f\\n' \| sort \| tr '\\n' ' '\)" = 'f:SHA256SUMS f:image\.tar f:metadata\.json '/)
  assert.match(workflow, /docker load --input image\.tar/)
  assert.match(workflow, /docker tag "\$LOCAL_IMAGE" "\$STAGING_TAG"/)
  assert.match(workflow, /docker push "\$STAGING_TAG"/)
  assert.match(workflow, /\.config\.digest.*"\$IMAGE_ID"/s)
  assert.doesNotMatch(workflow, /actions\/download-artifact|docker\/build-push-action|build-args:|context: \./)
})

test('publication stages the exact CI image before mandatory Cosign verification', () => {
  const push = workflow.indexOf('Push the exact tested CI candidate to the staging tag')
  const sign = workflow.indexOf('Sign attest and strictly verify the staged digest with GitHub OIDC')
  const publish = workflow.indexOf('Publish only the immutable commit tag')
  assert.ok(push >= 0 && sign > push && publish > sign)
  const pushStep = stepSection(workflow, 'Push the exact tested CI candidate to the staging tag', 'Require staging tag to resolve to the built digest')
  assert.match(pushStep, /docker tag "\$LOCAL_IMAGE" "\$STAGING_TAG"/)
  assert.match(pushStep, /docker push "\$STAGING_TAG"/)
  assert.doesNotMatch(pushStep, /final_tag|imagetools create/)
  const signingStep = stepSection(workflow, 'Sign attest and strictly verify the staged digest with GitHub OIDC', 'Generate GitHub NEXT predicate attestation when enabled')
  assert.match(signingStep, /cosign sign --yes "\$subject"/)
  assert.match(signingStep, /cosign attest --yes/)
  assert.match(signingStep, /--type "\$NEXT_PREDICATE_TYPE"/)
  assert.match(signingStep, /release-next\.yml@\$\{GITHUB_REF\}/)
  assert.match(signingStep, /--certificate-github-workflow-sha "\$GITHUB_SHA"/)
  assert.doesNotMatch(signingStep, /imagetools create|FINAL_TAG/)
})

test('custom predicate binds CI artifacts and accepts one exact verified statement', () => {
  const predicate = stepSection(workflow, 'Create canonical NEXT-only predicate', 'Sign attest and strictly verify the staged digest with GitHub OIDC')
  assert.match(predicate, /jq -nS/)
  assert.match(predicate, /schemaVersion: 1/)
  assert.match(predicate, /revision: \$revision/)
  assert.match(predicate, /sha: \$releaseWorkflowSha/)
  assert.match(predicate, /path: \$ciWorkflow/)
  assert.match(predicate, /target: "next-production"/)
  assert.match(predicate, /mode: "standalone"/)
  assert.match(predicate, /baseImages:/)
  assert.match(predicate, /candidateArtifact:/)
  assert.match(predicate, /rollbackArtifact:/)
  assert.match(predicate, /tarSha256:/)
  assert.match(predicate, /imageTarSha256:/)
  assert.match(predicate, /metadataSha256:/)
  assert.match(predicate, /repository: \$image/)
  assert.match(predicate, /digest: \$imageDigest/)
  assert.doesNotMatch(predicate, /\bmoe\b|sekaitext|workspace|producer/i)

  const signing = stepSection(workflow, 'Sign attest and strictly verify the staged digest with GitHub OIDC', 'Generate GitHub NEXT predicate attestation when enabled')
  assert.match(signing, /@base64d \| fromjson/)
  assert.match(signing, /\$statement\._type == "https:\/\/in-toto\.io\/Statement\/v0\.1"/)
  assert.match(signing, /\$statement\.predicateType == \$predicateType/)
  assert.match(signing, /\$statement\.subject == \[\{name: \$image, digest: \{sha256: \$digest\}\}\]/)
  assert.match(signing, /\$statement\.predicate == \$expected\[0\]/)
  assert.match(signing, /any\(\$envelopes\[\];/)
})

test('Cosign installer and runtime are pinned to v3.1.2', () => {
  assert.match(workflow, /sigstore\/cosign-installer@[0-9a-f]{40}/)
  assert.match(workflow, /cosign-release: v3\.1\.2/)
  assert.match(workflow, /cosign version --json \| jq -r \.gitVersion/)
  assert.match(workflow, /= "v3\.1\.2"/)
})

test('GitHub custom attestation is optional but fail closed when enabled', () => {
  assert.doesNotMatch(workflow, /actions\/attest-build-provenance@/)
  assert.match(workflow, /actions\/attest@[0-9a-f]{40}/)
  assert.equal((workflow.match(/GITHUB_ARTIFACT_ATTESTATIONS_ENABLED == 'true'/g) || []).length, 1)
  assert.match(workflow, /push-to-registry: false/)
  assert.doesNotMatch(workflow, /push-to-registry: true/)
  assert.doesNotMatch(workflow, /continue-on-error/)
})

test('only one commit-addressed final tag is published and it is same-digest idempotent', () => {
  assert.match(workflow, /final_tag="\$\{image\}:next-\$\{GITHUB_SHA\}"/)
  assert.equal((workflow.match(/^\s*final_tag="/gm) || []).length, 1)
  assert.match(workflow, /docker buildx imagetools create --prefer-index=false --tag "\$FINAL_TAG"/)
  assert.match(workflow, /"\$IMAGE_DIGEST"\)/)
  assert.match(workflow, /immutable commit tag already points to a different digest/)
  const publish = stepSection(workflow, 'Publish only the immutable commit tag', 'Output final immutable image digest')
  assert.match(publish, /git\/ref\/heads\/main/)
  assert.match(publish, /test "\$current_tip" = "\$GITHUB_SHA"/)
  assert.match(publish, /test "\$\(inspect_tag\)" = "\$IMAGE_DIGEST"/)
  assert.doesNotMatch(workflow, /(?:^|:)latest(?:\s|$)/m)
  assert.doesNotMatch(workflow, /\bdeploy(?:ment)?\b/i)
})

test('every external action is pinned to a full commit SHA', () => {
  assertActionsPinned(workflow, 'release-next.yml')
  assertActionsPinned(ciWorkflow, 'ci.yml')
})

test('release and operations documentation names only the standalone contract', () => {
  for (const [name, contents] of [
    ['STANDALONE_RELEASE.md', releaseDocumentation],
    ['PRODUCTION_CONTRACT.md', productionContract],
    ['ROLLBACK_RUNBOOK.md', rollbackRunbook],
    ['README.md', readme],
  ]) {
    assert.doesNotMatch(contents, /release-paired\.yml|PAIRED_RELEASE\.md/, `${name} has a retired release reference`)
  }
  assert.match(releaseDocumentation, /release-next\.yml/)
  assert.match(releaseDocumentation, /next-<next-full-sha>/)
  assert.match(releaseDocumentation, /WORKSPACE_MODE=disabled/)
  assert.match(releaseDocumentation, /WEB_DIR.*\/app\/web/)
  assert.match(releaseDocumentation, /DB_PATH.*\/data\/moesekai\.db/)
  assert.match(releaseDocumentation, /DATA_DIR.*\/data/)
  assert.match(releaseDocumentation, /TZ.*UTC/)
  assert.match(releaseDocumentation, /GHCR_RELEASE_USERNAME/)
  assert.match(releaseDocumentation, /GHCR_RELEASE_TOKEN/)
  assert.match(releaseDocumentation, /packages: none/)
  assert.match(rollbackRunbook, /commit-addressed/)
  assert.match(productionContract, /standalone production image/i)
  assert.match(productionContract, /lyrics_peer_renditions_and_localizations/)
  assert.match(productionContract, /contracts\/public-lyrics\/v3\//)
  assert.match(readme, /STANDALONE_RELEASE\.md/)
})
