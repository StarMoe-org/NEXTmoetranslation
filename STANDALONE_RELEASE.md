# NEXT Standalone Image Release Runbook

## Scope

`.github/workflows/release-next.yml` is the only production image publication workflow. It is manually dispatched with no user-supplied source or artifact inputs. The workflow publishes the repository's own standalone NextTrans image and does not fetch, authenticate to, tag against, or package any external frontend release.

The selected revision must be the exact commit checked out from the repository default branch. A read-only `prepare` job resolves the latest matching `push` CI attempt and requires that exact attempt to have completed successfully, then validates the candidate/rollback artifact IDs, GitHub archive digests, candidate file hashes, rollback tar hash/content, and digest-qualified base images before any approval is requested. Only the `publish` job is protected by the `production` environment, so reviewers approve the exact inputs that the later job consumes; the publish job rechecks the live `main` tip after approval. Configure the environment to allow only the protected default branch and require the intended reviewers, and freeze protected `main` updates from approval through final-tag creation because GitHub does not offer an atomic branch-tip check plus registry-tag write.

The workflow publishes an image digest, signature, and attestations. It does not change a runtime service.

## Required Repository Configuration

Repository variables used by push CI to build the candidate exactly once:

| Name | Requirement |
| --- | --- |
| `NODE_IMAGE_DIGEST` | Approved 64-character lowercase SHA-256 for `node:20.19.4-alpine3.22`. |
| `GO_IMAGE_DIGEST` | Approved 64-character lowercase SHA-256 for `golang:1.25.1-alpine3.22`. |
| `RUNTIME_IMAGE` | Approved lowercase runtime image name without an embedded `@` digest. The image must already contain pinned Git, CA certificates, and timezone data. |
| `RUNTIME_IMAGE_DIGEST` | Approved 64-character lowercase SHA-256 for `RUNTIME_IMAGE`. |
| `GITHUB_ARTIFACT_ATTESTATIONS_ENABLED` | Optional. Set exactly `true` only when the repository and plan support GitHub artifact attestations. Omit or set false otherwise. |

The Dockerfile also carries the same reviewed image names and immutable digests as defaults so direct source builders such as Zeabur do not expand a `FROM` instruction to an empty image or digest. These defaults are source-build compatibility values only. Push CI continues to pass the four repository variables explicitly, validate their exact shape and approved image references, and build the release candidate once; the release workflow still rebuilds nothing and promotes only those validated candidate bytes.

Do not shadow the four base-image variables in the `production` environment. Release never rebuilds and does not read them; it downloads the exact tested CI candidate, validates its canonical metadata and checksums, and promotes those bytes. CI uploads both `next-production-candidate-<sha>-<attempt>` and `rollback-<sha>-<attempt>` with 90-day retention. Complete rollout records must archive both artifacts durably before that retention window expires.

Configure these values only on the protected `production` environment, never as repository-wide values:

| Name | Kind | Requirement |
| --- | --- | --- |
| `GHCR_RELEASE_USERNAME` | Environment variable | Dedicated machine account that owns the package-writer credential. |
| `GHCR_RELEASE_TOKEN` | Environment secret | Revocable least-privilege GHCR writer token for that account. Grant package read/write only; do not grant delete or unrelated repository/organization administration. |

The publish job explicitly gives its ordinary `GITHUB_TOKEN` `packages: none`; that token remains limited to source/artifact reads plus OIDC and GitHub attestation creation. Docker and Cosign authenticate to GHCR only with the protected environment credential. The optional GitHub duplicate attestation is stored by GitHub with `push-to-registry: false`, so it does not require package-write authority on `GITHUB_TOKEN`.

Registry and Actions controls are mandatory:

1. Configure `production` to accept protected `main` only and require the intended reviewer before environment variables or secrets are released.
2. At the organization/repository policy boundary, prevent ordinary repository `GITHUB_TOKEN`s from obtaining package-write authority. A branch-modified workflow must not be able to replace the protected environment credential with `packages: write`. If the current GitHub policy cannot enforce that boundary, publish from a separate locked publisher repository/package instead of this repository.
3. Restrict the dedicated environment credential to this package and protected release path.
4. Make the final commit-addressed tags immutable and non-deletable under ordinary release credentials.
5. Disable every other workflow or credential capable of writing the package, or require it to share the same repository-wide concurrency and immutable-tag policy.

Workflow concurrency prevents conforming release runs from racing one another. Environment branch protection and credential scoping prevent an unprotected selected-ref workflow from receiving the writer; registry immutability and writer restriction prevent a different writer from changing the final tag between its precheck and postcheck.

## Standalone Image Contract

The Dockerfile's default final stage is `next-production`, inherited from `standalone`. It permanently sets:

```text
MOESEKAI_PRODUCTION=true
WORKSPACE_MODE=disabled
```

`WORKSPACE_WEB_DIR` and `WORKSPACE_MANIFEST_SHA256` must remain unset. `WEB_DIR` is permanently pinned to the root-owned bundled console at `/app/web`, `DB_PATH` is pinned to `/data/moesekai.db`, `DATA_DIR` is pinned to `/data`, and `TZ` is pinned exactly to `UTC`; any missing value or deployment override fails during `--verify-runtime` before the persistent data path is touched. The server listens on `:8080` (all interfaces) by default. An unset `CONSOLE_ORIGIN` resolves to `*` so platform-assigned production domains can use the console without a baked hostname; operators may restrict it to one validated http(s) origin. Public `/files/*` and `/translation/*` assets independently retain wildcard CORS. Production startup also sets Go's process-local timezone to `time.UTC`, so a base image's `/etc/localtime` cannot change application-local time. No workspace build context or workspace bytes are copied. The final stage replaces the development server with a binary stamped at link time as `next-production`; that executable rejects any runtime attempt to unset or override `MOESEKAI_PRODUCTION=true`. The image build executes `moesekai-server --verify-runtime` under the complete baked production environment before the final stage is accepted. At container start, the entrypoint executes `moesekai-server --verify-runtime` before creating, chmodding, inspecting, or migrating the persistent data path; normal server startup repeats the same policy check. The server applies a bounded tolerant percent-decoder plus path normalization before generic OPTIONS handling and ServeMux canonicalization, so canonical, escaped-separator/backslash, valid or malformed nested encoding, encoded-dot-segment, and other noncanonical paths that enter `/workspace` return secure `404 no-store` responses and never fall through to the NextTrans admin console.

The runtime identity is `65532:65532`. `/app`, its binaries, and exported console files are root-owned and non-writable. Production starts with a read-only root filesystem, a writable persistent `/data` volume, and a bounded writable `/tmp` tmpfs. One process owns the SQLite location for its lifetime; a competing writer must fail before opening the database.

The server binary also embeds the accepted public-only Public Lyrics v3 runtime projection at `server/internal/publiclyricsbundle/public-v3.tar.gz`. Its archive SHA-256 is `ed983c0d5735cbffd253081aae8925cc58ca3878aae02d8bf6701f847a8475f1`; initial public projection and every later rebuild require the exact closed inventory of 692 JSON assets, 21,936,149 uncompressed bytes, 709 unique catalog records, 691 details, and the reviewed state counts. The package rejects any nested, duplicate, non-regular, oversized, unexpected, or identity-mismatched member. It contains only `index.json` and `music_<id>.json`: no candidate manifest, receipt, producer database, recovery evidence, or authenticated/private input. After every database-backed files-service rebuild, these immutable bytes atomically replace both canonical `translation/lyrics/*` assets and all supported `v2/{locale}/translation/lyrics/*` mirrors. The overlay does not migrate, replace, or write `/data/moesekai.db`.

The same `next-production` binary separately embeds the private editor seed at `server/internal/embeddedlyricsseed/editor-seed.tar.gz`, archive SHA-256 `a8a2a7c841d0d73e448fd69f9adb236965b3b01a89d2ba58dcc921925e6ea479`. It is not mounted or served as a public asset. After migrations and before settings/account environment seeds, startup defers an empty catalog for the existing first-boot bootstrap, but requires every nonempty catalog to be the exact canonical 700 and atomically fills only missing lyrics ownership with 652 native source-v3 documents, one legacy draft, and 47 text-free availability states. Existing legacy/source/recovery-availability ownership is preserved without modification; replay verifies the immutable batch/ledger and inserted ownership while creating zero new rows. Catalog count/title/fingerprint drift, a different seed batch, or replay ownership loss fails startup. The seed excludes raw provider/recovery evidence, producer SQLite bytes, credentials, accounts, settings, ordinary translations, stories, reviews, and publications.

CI builds this exact production target and verifies the permanent environment, absence of `/app/workspace`, read-only application tree, UID/GID, fail-closed production secrets and administrator initialization, disabled `/workspace*` responses, the single-writer lock, persistent restart, bounded clean shutdown, and both content-addressed embedded lyrics bundle contracts.

## Publication Sequence

1. The read-only `prepare` job requires the repository default branch and dispatch ref to be exactly `main`, queries the live branch tip, selects the latest matching `push` CI attempt for `github.sha`, and requires that exact latest attempt to be completed successfully.
2. Resolve exactly one unexpired candidate artifact and one rollback artifact whose names bind that CI SHA and attempt. Require canonical GitHub SHA-256 artifact digests.
3. Before approval, download both artifacts by immutable artifact ID, compare each downloaded ZIP byte digest with the resolved GitHub digest, require closed outer inventories, validate candidate `SHA256SUMS` and complete metadata shape, safely inspect the rollback tar's closed path/type/mode/checksum contract, and record the exact CI URL, artifact IDs/digests, candidate file hashes, rollback tar SHA-256, and digest-qualified bases in the job summary.
4. Only then may the protected `publish` job receive `production` approval. It consumes the immutable prepare outputs rather than selecting a newer same-SHA rerun, checks out `github.sha` with credentials disabled, rechecks live `main`, redownloads both candidate and rollback artifacts by their approved IDs, repeats both GitHub archive-digest and closed-content validations, requires candidate file hashes/bases and rollback tar SHA-256 to equal the preapproval values, loads `image.tar`, and verifies the loaded image ID, canonical source label, UID/GID, production environment, pinned `/app/web`, pinned `/data/moesekai.db`, pinned `DATA_DIR=/data`, exact `TZ=UTC`, UTC zoneinfo data, and workspace-disabled state.
5. Install exact Cosign v3.1.2 and require its reported runtime version to match.
6. Derive the lowercase GHCR repository, one run-scoped staging tag, and one final commit tag:

```text
ghcr.io/<next-repository>:staging-<github-run-id>-<github-run-attempt>
ghcr.io/<next-repository>:next-<next-full-sha>
```

7. Require the staging tag to be unused, authenticate Docker/Cosign with the protected `GHCR_RELEASE_USERNAME`/`GHCR_RELEASE_TOKEN` environment credential, retag the loaded CI image, and push it without invoking Docker build or reading release-time base variables. Require the remote manifest's config digest to equal the loaded CI image ID and record the resulting manifest digest.
8. Require the staging tag to resolve to that exact `sha256:` manifest digest.
9. Generate a canonical NEXT-only JSON predicate with `jq -nS`. It records only the NEXT source repository/revision/ref, exact release workflow path and SHA, exact successful CI run, candidate and rollback artifact IDs/names/archive digests, candidate file hashes, rollback tar SHA-256, standalone target/mode, CI-recorded digest-qualified bases, and image repository/digest. It contains no invented external-source fields.
10. Keylessly sign the digest with GitHub OIDC, attach the custom predicate, and verify the signature and attestation with:

```text
OIDC issuer: https://token.actions.githubusercontent.com
certificate identity: https://github.com/<next-repository>/.github/workflows/release-next.yml@refs/heads/<default-branch>
GitHub workflow SHA: <next-full-sha>
predicate type: https://github.com/<next-repository>/attestations/next-production-image/v1
```

11. Decode the cryptographically verified DSSE statements and require at least one exact in-toto statement whose predicate type, image subject/digest, and complete predicate JSON match the local expected predicate. Historical valid statements may coexist; they cannot substitute for the exact current statement.
12. When `GITHUB_ARTIFACT_ATTESTATIONS_ENABLED=true`, also publish the same custom predicate through GitHub's artifact-attestation service with registry push disabled. This optional duplicate publication fails closed when explicitly enabled; it is not labeled build provenance because the image was built in push CI, not in the promotion job, and it does not require package-write authority on `GITHUB_TOKEN`.
13. While the operator-enforced protected-branch freeze remains active, re-query live `refs/heads/main` immediately before final-tag creation and require it still equals the release SHA. Publish only `next-<next-full-sha>`. If it already exists, it must resolve to the exact verified digest; a different digest fails. Reinspect the final tag after creation. No `latest`, branch, SemVer, or external-artifact tag is created.

A failure before the final-tag request leaves no newly created final tag. Once registry creation is attempted, a client or post-inspection failure is an ambiguous outcome: inspect `next-<next-full-sha>` by digest before retrying. The run-scoped staging tag is not a rollout selector.

## Operator Procedure

1. Confirm the intended full commit is the current protected `main` revision and that its exact push CI run passed.
2. Inspect that CI run's candidate and rollback artifact names, IDs, SHA-256 artifact digests, rollback tar SHA-256/content verification, 90-day expiry, candidate metadata, and digest-qualified base images. Do not approve if either artifact is missing, expired, mutable, malformed, or from another attempt.
3. Confirm the protected production environment contains the dedicated GHCR writer credential, ordinary repository `GITHUB_TOKEN` package writes are disabled by policy, and final-tag immutability is active.
4. Freeze protected `main` updates, dispatch `Release NEXT standalone image` on `main`, wait for the read-only `prepare` job to finish, and approve the `production` environment only after reviewing its exact commit, CI attempt, candidate/rollback artifact IDs and archive digests, candidate file hashes, and bases. Keep the freeze active until final-tag verification completes; the workflow rechecks the live main tip after approval and again inside final publication.
5. Record the final image digest, `next-<full-sha>` tag, workflow run URL, exact successful CI run URL, candidate and rollback artifact records, base digests, signature evidence, and custom predicate. Durably archive the two CI artifacts before rollout and before their retention expires.
6. Independently verify the digest-qualified image signature and predicate before a separate rollout process selects the digest.

Run local release checks with:

```bash
node --test scripts/release-workflow.test.mjs
./scripts/verify-release.sh
git diff --check
```

These local checks do not log in to a registry, request OIDC credentials, push an image, dispatch a workflow, or modify a running service.
