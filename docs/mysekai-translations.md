# MYSEKAI translation workflow

MYSEKAI translations have their own source and publication identities. Editors
change display text; Lua, voices, staging and control flow stay with the selected
game region. Chinese hosts resolve matching official CN text first, published
human translations second, and Japanese source text last. Official pairing belongs
to the extractor; this service never guesses a match from a talk ID.

## For translators

1. Open **MYSEKAI → 对话翻译与发布** and choose the editing language.
2. Search a conversation or use the untranslated / changed-source filter.
3. Edit text and save a draft, or download its TXT template. In the template,
   change only text between `<<<译文` and `>>>结束`; keep its metadata unchanged.
4. Upload the TXT to preview differences. Empty entries do not erase work.
   Select replacements explicitly. If any selected entry changed on the server,
   the entire save is rejected; reload and review before retrying.
5. **舞台预览已保存草稿** opens pjsk.moe with an isolated draft text session.
   Click play there. Nothing is published by previewing. Unsupported content
   families show an explanation instead of opening an unrelated scene.
6. An administrator publishes reviewed drafts. A pending response means the DB
   write succeeded but the public projection is still rebuilding. Check publication
   status before announcing availability.

Source updates preserve drafts and edit history. A changed `sourceHash` requires
review before that draft can be published again. Removed source remains archived.
New official text does not delete human work. Newlines are supported; tabs and
control characters are rejected. Text is rendered as text, never as HTML.

## Source synchronization and API

All APIs require the existing authenticated console session. Writes use the
producer-aware `/api/editor/v1` gate. Console writes include the loaded producer
state header and per-content revisions.

| Method and path | Role | Purpose |
|---|---|---|
| GET `/api/mysekai/scripts` | editor | `region`, `locale`, `q`, `state`, `cursor`, `limit`; summaries and source/draft/channel revisions |
| GET `/api/mysekai/script` | editor | `region`, `locale`, `key`; source, linked tweets and drafts |
| POST `/api/editor/v1/mysekai/source` | admin | `{catalog, expectedRevision}`; full `moly-text-source/1` from moly-root |
| PUT `/api/editor/v1/mysekai/drafts` | editor | `{region, locale, scriptKey, sourceRevision, edits:[{id,sourceHash,text,expectedRevision}]}` |
| POST `/api/editor/v1/mysekai/publication/{action}` | admin | action `publish`, `unpublish`, `rollback`; channel `expectedRevision`, `sourceRevision`, `draftRevision`, optional rollback `publication` |
| GET `/api/mysekai/publications` | editor | `region`, `locale`; latest 100 pointer changes |

Source imports are capped at 64 MiB. Normal writes retain the shared request limit.
A reused source revision cannot carry different catalog bytes. Source imports and
batch edits commit atomically. Publication checks the administrator's loaded draft
revision, so a concurrent editor cannot silently add unreviewed work to a release.

## Public files and cache behavior

The public entry is `/files/mysekai/{region}/{locale}/latest.json`:

```json
{"schema":"moly-text-publication/1","region":"jp","locale":"zh-CN","revision":"<sha256>","routing":"sha256-utf8-prefix-2","buckets":{"ab":{"path":"objects/<sha256>.json","sha256":"<sha256>","bytes":123}}}
```

Hash the UTF-8 script key with SHA-256 and use its first two hex characters to
select the bucket. A `moly-text-translation-routes/1` bucket has a `scripts` map
from exact script key to the same `{path,sha256,bytes}` reference. Each leaf is a
`moly-text-overlay/1` with `sourceRegion`, `sourceRevision`, `locale`, `revision`
and `entries:[{id,sourceHash,text,source:"human"}]`. Linked tweet translations are
included in their conversation's leaf. All paths are relative to the region/locale
directory. Verify bytes and hash before parsing.

Leaves hash their own entries and source projection; a global registry change
does not rewrite every leaf. Changed text updates only affected leaves, routing
buckets and the discovery root. Game resource snapshots remain independent.
Consumers admit lines by stable ID and `sourceHash`, not global source revision.

`latest.json` uses a 15-second revalidation policy. Immutable objects and
`revisions/{revision}.json` use a one-year immutable cache policy and stay available
after later releases, withdrawal and rollback. A playback session pins its chosen
source and translation versions. Withdrawing a publication removes discovery;
it does not destroy immutable history or interrupt pinned sessions.

The shared file-service map switches only after a complete successful rebuild.
A failed rebuild keeps the previous map. Database publication is therefore distinct
from projection readiness. Backups use content schema 3 and include source versions,
current source, drafts, edit history, immutable releases and pointer history.
Immutable bucket and leaf bytes are stored once by SHA-256 in SQLite and in the
backup; each release stores only its root manifest. A one-line correction does
not duplicate the previous complete object set.
Schemas 1 and 2 remain readable, with MYSEKAI empty when absent. Restore validates
the publication reference graph and hashes within the content transaction.

## Stage preview integration

Set `NEXT_PUBLIC_MOESEKAI_ORIGIN` at console build time (default
`https://pjsk.moe`). The host configures its trusted translation-console origin.
Preview opens `/mysekai/interactions/?region=jp&content=talk:common:1&translationPreview=<nonce>`
from the click gesture; general and fixture keys have the same three-part shape.

The host sends `{source:"moly-preview",type:"ready",nonce}` to its opener. The
console checks origin, exact window and nonce, then sends
`{source:"nexttrans-mysekai",type:"preview",nonce,region,content,session:{source,official:"",community,locale}}`.
`source` and `community` are JSON strings, at most 2 MiB each. The listener expires
after 60 seconds and carries no account token. The host must validate the same
window/origin/nonce and current source identities before installing the session.
Draft preview never writes its public pointer or starts audio automatically.

Optional future font references may use the same immutable `{path,sha256,bytes}`
contract. Font administration is separate from this text editor.
