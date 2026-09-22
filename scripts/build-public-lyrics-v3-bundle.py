#!/usr/bin/env python3
"""Build the deterministic, public-only Public Lyrics v3 runtime bundle."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import re
import tarfile
import tempfile
from pathlib import Path

# The reviewed artifact identity, batch binding and counts live in one place;
# only the private accepted-candidate digests stay here, because nothing else
# in the repository refers to them.
BASELINE = json.loads((Path(__file__).resolve().parent.parent / "contracts" / "public-lyrics" / "baseline.json").read_text(encoding="utf-8"))
BUNDLE_PINS = BASELINE["publicLyricsBundle"]
EXPECTED_BATCH = BUNDLE_PINS["batchSha256"]
EXPECTED_ROOT = BUNDLE_PINS["rootSha256"]
EXPECTED_CATALOG = BUNDLE_PINS["catalogCount"]
EXPECTED_DETAILS = BUNDLE_PINS["detailCount"]
EXPECTED_ASSETS = BUNDLE_PINS["memberCount"]
EXPECTED_ARCHIVE_SHA256 = BUNDLE_PINS["archiveSha256"]
EXPECTED_MANIFEST_SHA256 = "b88f3076e40a6711b9e6a55321ede9da0aef0b69489a22b5b74fe468f5676d6f"
EXPECTED_RECEIPT_FILE_SHA256 = "a4bf207f446feffd71f2e51ab1755ac3c9cd648b34fe72596f85de3c6a559deb"
EXPECTED_RECEIPT_SHA256 = "fddf772043e1fa4a70e0bc677ada44e61121ef9c6ef1ccf7e04c419c789b039d"
EXPECTED_CONTENT_SHA256 = "6e0395c926470c591f70195aa6cf96ed6df1ea961b54d6a9fb6229f4bbe3d4b2"
DETAIL_RE = re.compile(r"music_([1-9][0-9]*)\.json")


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("source", type=Path, help="accepted hardened v3 candidate directory")
    parser.add_argument("output", type=Path, help="output public-v3.tar.gz")
    args = parser.parse_args()

    source = args.source.resolve()
    output = args.output.resolve()
    manifest_path = source / "manifest.json"
    receipt_path = source / "receipt.json"
    if not manifest_path.is_file() or not receipt_path.is_file():
        raise SystemExit("source must contain the accepted manifest and receipt")

    manifest_bytes = manifest_path.read_bytes()
    receipt_bytes = receipt_path.read_bytes()
    if sha256(manifest_bytes) != EXPECTED_MANIFEST_SHA256 or sha256(receipt_bytes) != EXPECTED_RECEIPT_FILE_SHA256:
        raise SystemExit("candidate manifest/receipt identity differs")
    manifest = json.loads(manifest_bytes)
    receipt = json.loads(receipt_bytes)
    if manifest.get("batchSha256") != EXPECTED_BATCH or manifest.get("rootSha256") != EXPECTED_ROOT:
        raise SystemExit("candidate batch/root binding differs")
    if manifest.get("contentSha256") != EXPECTED_CONTENT_SHA256:
        raise SystemExit("candidate content binding differs")
    if manifest.get("publicLyricsVersion") != 3 or manifest.get("catalogCount") != EXPECTED_CATALOG or manifest.get("detailCount") != EXPECTED_DETAILS:
        raise SystemExit("candidate public counts differ")
    if (
        receipt.get("batchSha256") != EXPECTED_BATCH
        or receipt.get("rootSha256") != EXPECTED_ROOT
        or receipt.get("contentSha256") != EXPECTED_CONTENT_SHA256
        or receipt.get("catalogCount") != EXPECTED_CATALOG
        or receipt.get("detailCount") != EXPECTED_DETAILS
        or receipt.get("assetCount") != EXPECTED_ASSETS
        or receipt.get("manifestSha256") != EXPECTED_MANIFEST_SHA256
        or receipt.get("manifestBytes") != len(manifest_bytes)
        or receipt.get("receiptSha256") != EXPECTED_RECEIPT_SHA256
    ):
        raise SystemExit("candidate receipt binding differs")

    runtime_paths = [Path("index.json")]
    runtime_paths.extend(
        sorted(
            (path.relative_to(source) for path in source.glob("music_*.json") if DETAIL_RE.fullmatch(path.name)),
            key=lambda path: int(DETAIL_RE.fullmatch(path.name).group(1)),
        )
    )
    if len(runtime_paths) != EXPECTED_ASSETS:
        raise SystemExit(f"runtime asset count={len(runtime_paths)} expected={EXPECTED_ASSETS}")

    manifest_hashes = {manifest["index"]["path"]: manifest["index"]["sha256"]}
    manifest_hashes.update({entry["path"]: entry["sha256"] for entry in manifest["details"]})
    if set(manifest_hashes) != {path.as_posix() for path in runtime_paths}:
        raise SystemExit("manifest/runtime inventory differs")

    assets: list[tuple[str, bytes, str]] = []
    for relative in runtime_paths:
        data = (source / relative).read_bytes()
        digest = sha256(data)
        if manifest_hashes[relative.as_posix()] != digest:
            raise SystemExit(f"asset hash differs: {relative}")
        assets.append((relative.as_posix(), data, digest))

    index = json.loads(assets[0][1])
    if index.get("version") != 3 or len(index.get("songs", [])) != EXPECTED_CATALOG:
        raise SystemExit("index version/catalog count differs")

    inventory = hashlib.sha256()
    for path, data, digest in sorted(assets):
        inventory.update(path.encode("utf-8"))
        inventory.update(b"\0")
        inventory.update(digest.encode("ascii"))
        inventory.update(b"\0")
        inventory.update(str(len(data)).encode("ascii"))
        inventory.update(b"\n")

    output.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(prefix=output.name + ".", dir=output.parent)
    os.close(fd)
    temporary = Path(temporary_name)
    try:
        with temporary.open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", compresslevel=9, fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.GNU_FORMAT) as archive:
                    for path, data, _ in assets:
                        info = tarfile.TarInfo(path)
                        info.size = len(data)
                        info.mode = 0o444
                        info.uid = 0
                        info.gid = 0
                        info.uname = ""
                        info.gname = ""
                        info.mtime = 0
                        info.type = tarfile.REGTYPE
                        archive.addfile(info, fileobj=io.BytesIO(data))
        archive_data = temporary.read_bytes()
        runtime_bytes = sum(len(data) for _, data, _ in assets)
        os.chmod(temporary, 0o644)
        os.replace(temporary, output)
    finally:
        temporary.unlink(missing_ok=True)

    archive_sha256 = sha256(archive_data)
    if archive_sha256 != EXPECTED_ARCHIVE_SHA256:
        raise SystemExit(
            f"built archive {archive_sha256} differs from contracts/public-lyrics/baseline.json "
            f"({EXPECTED_ARCHIVE_SHA256}); update the baseline in the same commit as a new reviewed bundle"
        )

    print(f"archive_sha256={archive_sha256}")
    print(f"archive_bytes={len(archive_data)}")
    print(f"inventory_sha256={inventory.hexdigest()}")
    print(f"runtime_bytes={runtime_bytes}")
    print(f"assets={len(assets)} catalog={EXPECTED_CATALOG} details={EXPECTED_DETAILS}")


if __name__ == "__main__":
    main()
