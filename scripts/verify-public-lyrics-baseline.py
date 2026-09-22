#!/usr/bin/env python3
"""Verify the committed public lyrics artifacts against contracts/public-lyrics/baseline.json.

This checks the artifacts that ship inside the binary. It does not re-run the
builders: build-public-lyrics-v3-bundle.py needs the accepted candidate
directory and build-embedded-lyrics-editor-seed.py needs the accepted producer
database, neither of which is part of this repository.
"""

from __future__ import annotations

import gzip
import hashlib
import io
import json
import re
import sys
import tarfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BASELINE_PATH = ROOT / "contracts" / "public-lyrics" / "baseline.json"
DETAIL_RE = re.compile(r"music_([1-9][0-9]*)\.json\Z")
FORBIDDEN_FIELDS = (
    b'"databaseSha256"', b'"manifestSha256"', b'"receiptSha256"', b'"rawBytes"',
    b'"privateReview"', b'"indexEvidenceRefs"', b'"documentJson"', b'"fixedIdentityJson"',
    b'"sourceUrl"', b'"sourceSha1"', b'"sourceFetchedAt"', b'"acquisitionId"',
)


def load_baseline() -> dict:
    return json.loads(BASELINE_PATH.read_text(encoding="utf-8"))


def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise SystemExit(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def verify_archive_identity(path: Path, expected_sha256: str) -> bytes:
    data = path.read_bytes()
    actual = hashlib.sha256(data).hexdigest()
    if actual != expected_sha256:
        raise SystemExit(f"{path}: SHA-256 {actual} differs from the baseline pin {expected_sha256}")
    return data


def verify_bundle(pins: dict, archive: bytes) -> None:
    with tarfile.open(fileobj=io.BytesIO(gzip.decompress(archive)), mode="r:") as tar:
        members = tar.getmembers()
        bodies = {member.name: tar.extractfile(member).read() for member in members}
    if len(members) != pins["memberCount"]:
        raise SystemExit(f"bundle member count={len(members)}, baseline={pins['memberCount']}")
    names = [member.name for member in members]
    if len(names) != len(set(names)) or names.count("index.json") != 1:
        raise SystemExit("bundle inventory is duplicated or lacks exactly one index")
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
        raise SystemExit("bundle contains noncanonical tar metadata")
    detail_ids = [int(match.group(1)) for name in names if (match := DETAIL_RE.fullmatch(name))]
    if len(detail_ids) != pins["detailCount"] or len(detail_ids) != len(set(detail_ids)):
        raise SystemExit(f"bundle detail count={len(detail_ids)}, baseline={pins['detailCount']}")
    if set(names) != {"index.json", *(f"music_{music_id}.json" for music_id in detail_ids)}:
        raise SystemExit("bundle contains a nested, private, or unexpected artifact")

    index = json.loads(bodies["index.json"], object_pairs_hook=reject_duplicates)
    if index.get("version") != 3:
        raise SystemExit("bundle index is not a public lyrics v3 document")
    if len(index["songs"]) != pins["catalogCount"]:
        raise SystemExit(f"bundle catalog count={len(index['songs'])}, baseline={pins['catalogCount']}")
    expected_details = {song["musicId"] for song in index["songs"] if song["state"] in {"complete", "game_only"}}
    if expected_details != set(detail_ids):
        raise SystemExit("bundle index/detail identity differs")
    for music_id in detail_ids:
        document = json.loads(bodies[f"music_{music_id}.json"], object_pairs_hook=reject_duplicates)
        if document.get("version") != 3 or document.get("musicId") != music_id:
            raise SystemExit(f"bundle detail identity differs: {music_id}")
    for forbidden in FORBIDDEN_FIELDS:
        if any(forbidden in body for body in bodies.values()):
            raise SystemExit(f"bundle contains forbidden private field {forbidden!r}")


def main() -> None:
    baseline = load_baseline()
    bundle_pins = baseline["publicLyricsBundle"]
    seed_pins = baseline["embeddedEditorSeed"]
    archive = verify_archive_identity(ROOT / bundle_pins["path"], bundle_pins["archiveSha256"])
    verify_bundle(bundle_pins, archive)
    verify_archive_identity(ROOT / seed_pins["path"], seed_pins["archiveSha256"])
    print(f"public lyrics baseline verified: {bundle_pins['path']}, {seed_pins['path']}")


if __name__ == "__main__":
    sys.exit(main())
