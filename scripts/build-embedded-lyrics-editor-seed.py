#!/usr/bin/env python3
"""Build the deterministic private 700-song editor seed from the accepted DB."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import sqlite3
import tarfile
from pathlib import Path

SCHEMA_VERSION = 1
RELEASE_ID = "runtime-rebind-700-editor-seed-final-20260808b"
# The producer database identity, the accepted batch binding and the seed
# archive identity are pinned once for every builder, verifier and CI step.
BASELINE = json.loads((Path(__file__).resolve().parent.parent / "contracts" / "public-lyrics" / "baseline.json").read_text(encoding="utf-8"))
SOURCE_BATCH = BASELINE["publicLyricsBundle"]["batchSha256"]
ROOT_SHA = BASELINE["publicLyricsBundle"]["rootSha256"]
CATALOG_POLICY = "catalog-identity-v2"
EXPECTED_DB_SHA256 = BASELINE["embeddedEditorSeed"]["producerDatabaseSha256"]
EXPECTED_ARCHIVE_SHA256 = BASELINE["embeddedEditorSeed"]["archiveSha256"]
EXPECTED = {
    "catalog": 700,
    "source_v3": 652,
    "legacy": 1,
    "availability": 47,
    "artifacts": 785,
    "contributions": 4893,
}

FILES = [
    "source-documents.json",
    "source-artifacts.json",
    "source-contributions.json",
    "legacy-documents.json",
    "legacy-lines.json",
    "legacy-segments.json",
    "availability-documents.json",
]


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def compact_json(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=False).encode("utf-8")


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def rows(connection: sqlite3.Connection, sql: str, names: list[str]) -> list[dict[str, object]]:
    result: list[dict[str, object]] = []
    for values in connection.execute(sql):
        if len(names) != len(values):
            raise SystemExit("seed query column count differs")
        result.append(dict(zip(names, values)))
    return result


def seed_sha256(items: list[dict[str, object]]) -> str:
    digest = hashlib.sha256()
    digest.update(f"{SCHEMA_VERSION}\0{RELEASE_ID}\0{SOURCE_BATCH}\0{ROOT_SHA}\0{CATALOG_POLICY}\n".encode())
    for item in items:
        digest.update(
            (
                f"{item['musicId']}\0{item['japaneseTitle']}\0{item['catalogFingerprint']}\0"
                f"{item['state']}\0{item['seedKind']}\0{item['resultSha256']}\0"
                f"{item.get('documentSha256', '')}\0{item.get('availabilitySha256', '')}\0{item['createdAt']}\n"
            ).encode("utf-8")
        )
    return digest.hexdigest()


def build(database: Path, output: Path) -> None:
    if file_sha256(database) != EXPECTED_DB_SHA256:
        raise SystemExit("accepted producer DB SHA-256 differs")
    connection = sqlite3.connect(f"file:{database}?mode=ro&immutable=1", uri=True)
    connection.execute("PRAGMA query_only=ON")
    connection.row_factory = sqlite3.Row
    integrity = [row[0] for row in connection.execute("PRAGMA integrity_check")]
    if integrity != ["ok"]:
        raise SystemExit(f"producer DB integrity check failed: {integrity}")
    batch = connection.execute(
        "SELECT batch_sha256,root_sha256,catalog_count,created_at FROM lyrics_recovery_import_batches"
    ).fetchone()
    if batch is None or batch[0] != SOURCE_BATCH or batch[1] != ROOT_SHA or batch[2] != EXPECTED["catalog"]:
        raise SystemExit("accepted recovery batch identity differs")
    created_at = int(batch[3])

    catalog_rows = connection.execute(
        """SELECT m.music_id,m.title_ja,m.lyrics_catalog_fingerprint,m.lyrics_catalog_policy_version,
                  i.state,i.result_sha256,i.document_sha256,i.availability_document_sha256,i.created_at,
                  CASE WHEN d.schema_version=3 THEN 'source_v3'
                       WHEN l.music_id IS NOT NULL THEN 'legacy'
                       ELSE 'availability' END AS seed_kind
           FROM catalog_music m
           JOIN lyrics_recovery_import_items i ON i.batch_sha256=? AND i.music_id=m.music_id
           LEFT JOIN song_lyrics_source_documents d ON d.music_id=m.music_id
           LEFT JOIN song_lyrics l ON l.music_id=m.music_id
           ORDER BY m.music_id""",
        (SOURCE_BATCH,),
    ).fetchall()
    if len(catalog_rows) != EXPECTED["catalog"]:
        raise SystemExit("producer catalog count differs")
    items: list[dict[str, object]] = []
    music_digest = hashlib.sha256()
    catalog_digest = hashlib.sha256()
    kind_counts: dict[str, int] = {}
    for row in catalog_rows:
        if row[3] != CATALOG_POLICY or int(row[8]) != created_at:
            raise SystemExit(f"catalog policy or item timestamp differs for music {row[0]}")
        kind = str(row[9])
        item: dict[str, object] = {
            "musicId": int(row[0]),
            "japaneseTitle": str(row[1]),
            "catalogFingerprint": str(row[2]),
            "state": str(row[4]),
            "seedKind": kind,
            "resultSha256": str(row[5]),
            "createdAt": int(row[8]),
        }
        if kind in {"source_v3", "legacy"}:
            item["documentSha256"] = str(row[6])
        else:
            item["availabilitySha256"] = str(row[7])
        items.append(item)
        kind_counts[kind] = kind_counts.get(kind, 0) + 1
        music_digest.update(f"{row[0]}\n".encode())
        catalog_digest.update(f"{row[0]}\0{row[2]}\n".encode())
    if kind_counts != {
        "source_v3": EXPECTED["source_v3"],
        "legacy": EXPECTED["legacy"],
        "availability": EXPECTED["availability"],
    }:
        raise SystemExit(f"producer seed partition differs: {kind_counts}")

    documents = rows(connection, """SELECT music_id,schema_version,reason_code,document_json,document_sha256,
                                                manifest_batch_sha256,created_at
                                         FROM song_lyrics_source_documents WHERE schema_version=3 ORDER BY music_id""",
                     ["musicId", "schemaVersion", "reasonCode", "documentJson", "documentSha256", "manifestBatchSha256", "createdAt"])
    artifacts = rows(connection, """SELECT a.music_id,a.provider,a.rendition_key,a.origin,a.page_id,a.revision_id,
                                              a.revision_timestamp,a.mediawiki_sha1,a.page_title,a.canonical_revision_url,
                                              a.fetched_at,a.categories_json,a.section,a.composition_rendition_key,
                                              a.version_reason,a.index_evidence_refs_json,a.fixed_identity_json,
                                              a.fixed_identity_sha256,a.raw_byte_count,a.raw_wikitext_sha256,a.artifact_sha256
                                       FROM lyrics_recovery_import_artifacts a
                                       JOIN song_lyrics_source_documents d
                                         ON d.manifest_batch_sha256=a.batch_sha256 AND d.music_id=a.music_id
                                       WHERE d.schema_version=3 ORDER BY a.music_id,a.rendition_key""",
                     ["musicId", "provider", "renditionKey", "origin", "pageId", "revisionId", "revisionTimestamp",
                      "mediawikiSha1", "pageTitle", "canonicalRevisionUrl", "fetchedAt", "categoriesJson", "section",
                      "compositionRenditionKey", "versionReason", "indexEvidenceRefsJson", "fixedIdentityJson",
                      "fixedIdentitySha256", "rawByteCount", "rawWikitextSha256", "artifactSha256"])
    contributions = rows(connection, """SELECT c.music_id,c.component,c.rendition_key,c.contribution_sha256
                                           FROM lyrics_recovery_import_component_contributions c
                                           JOIN song_lyrics_source_documents d
                                             ON d.manifest_batch_sha256=c.batch_sha256 AND d.music_id=c.music_id
                                           WHERE d.schema_version=3 ORDER BY c.music_id,c.component""",
                         ["musicId", "component", "renditionKey", "contributionSha256"])
    legacy_documents = rows(connection, """SELECT music_id,revision,updated_at,updated_by,attribution,translation_credit,
                                                      proofreading_credit,source_note,source_url,license_note,source_hash,
                                                      source_page_id,source_revision_id,source_sha1,source_fetched_at,
                                                      source_fetched_at_rfc3339 FROM song_lyrics ORDER BY music_id""",
                            ["musicId", "revision", "updatedAt", "updatedBy", "attribution", "translationCredit",
                             "proofreadingCredit", "sourceNote", "sourceUrl", "licenseNote", "sourceHash", "sourcePageId",
                             "sourceRevisionId", "sourceSha1", "sourceFetchedAt", "sourceFetchedAtRfc3339"])
    legacy_lines = rows(connection, """SELECT music_id,line_id,position,japanese,zh_cn,en_us,stanza_break_before
                                          FROM song_lyric_lines ORDER BY music_id,position""",
                        ["musicId", "lineId", "position", "japanese", "zh-CN", "en-US", "stanzaBreakBefore"])
    legacy_segments = rows(connection, """SELECT music_id,line_id,position,text,performer_ids_json,ruby_json
                                             FROM song_lyric_segments ORDER BY music_id,line_id,position""",
                           ["musicId", "lineId", "position", "text", "performerIdsJson", "rubyJson"])
    availability = rows(connection, """SELECT music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,
                                                  document_sha256,result_sha256,created_at
                                           FROM song_lyrics_availability_documents
                                           WHERE state IN ('satisfied_no_lyrics','ambiguous','missing','incomplete','failed')
                                           ORDER BY music_id""",
                        ["musicId", "schemaVersion", "state", "reasonCode", "noLyricsReason", "documentJson",
                         "documentSha256", "resultSha256", "createdAt"])
    connection.close()

    if len(documents) != EXPECTED["source_v3"] or len(artifacts) != EXPECTED["artifacts"] or len(contributions) != EXPECTED["contributions"]:
        raise SystemExit("producer source-v3 structural counts differ")
    if len(legacy_documents) != EXPECTED["legacy"] or len(availability) != EXPECTED["availability"]:
        raise SystemExit("producer legacy/availability counts differ")

    file_values = {
        "source-documents.json": documents,
        "source-artifacts.json": artifacts,
        "source-contributions.json": contributions,
        "legacy-documents.json": legacy_documents,
        "legacy-lines.json": legacy_lines,
        "legacy-segments.json": legacy_segments,
        "availability-documents.json": availability,
    }
    file_bodies = {name: compact_json(value) for name, value in file_values.items()}
    file_records = [
        {"name": name, "sha256": sha256(file_bodies[name]), "bytes": len(file_bodies[name]), "count": len(file_values[name])}
        for name in FILES
    ]
    seed_digest = seed_sha256(items)
    for document in documents:
        document["manifestBatchSha256"] = seed_digest
    file_bodies["source-documents.json"] = compact_json(documents)
    for record in file_records:
        if record["name"] == "source-documents.json":
            record["sha256"] = sha256(file_bodies[record["name"]])
            record["bytes"] = len(file_bodies[record["name"]])

    manifest = {
        "schemaVersion": SCHEMA_VERSION,
        "releaseId": RELEASE_ID,
        "sourceBatchSha256": SOURCE_BATCH,
        "rootSha256": ROOT_SHA,
        "catalogPolicyVersion": CATALOG_POLICY,
        "catalogCount": EXPECTED["catalog"],
        "musicIdsSha256": music_digest.hexdigest(),
        "catalogFingerprintsSha256": catalog_digest.hexdigest(),
        "seedSha256": seed_digest,
        "createdAt": created_at,
        "items": items,
        "files": file_records,
    }
    entries = {"manifest.json": compact_json(manifest), **file_bodies}
    tar_buffer = io.BytesIO()
    with tarfile.open(fileobj=tar_buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name in ["manifest.json", *FILES]:
            body = entries[name]
            info = tarfile.TarInfo(name)
            info.size = len(body)
            info.mode = 0o400
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            info.mtime = 0
            archive.addfile(info, io.BytesIO(body))
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=0) as compressed:
            compressed.write(tar_buffer.getvalue())
    archive_sha256 = file_sha256(output)
    if archive_sha256 != EXPECTED_ARCHIVE_SHA256:
        raise SystemExit(
            f"built seed {archive_sha256} differs from contracts/public-lyrics/baseline.json "
            f"({EXPECTED_ARCHIVE_SHA256}); update the baseline in the same commit as a new reviewed seed"
        )
    print(json.dumps({
        "output": str(output),
        "archiveSha256": archive_sha256,
        "seedSha256": seed_digest,
        "bytes": output.stat().st_size,
        "counts": {name: len(value) for name, value in file_values.items()},
    }, ensure_ascii=False, separators=(",", ":")))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    build(args.database.resolve(), args.output.resolve())


if __name__ == "__main__":
    main()
