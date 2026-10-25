# MYSEKAI translation fonts

Translators edit text, upload TXT, save drafts, preview, and request publication.
They never build or upload a font. The editor shows unsupported characters before
publication; drafts remain savable so the text is not lost.

An operator publishes the approved OFL TrueType font and its coverage catalogue
once, independently from scenes, translation revisions, and WASM. The layout is:

```
fonts/latest.json
fonts/revisions/<revision>.json
fonts/objects/<sha256>.ttf
fonts/objects/<license-sha256>.txt
```

Use the Moly root font-publication command and existing S3/OSS uploader. Publish
immutable font, licence, and history objects before moving `latest.json`. Keep
the licence with the font, retain historical objects, and allow anonymous CORS
reads from the player origin. Immutable objects use a one-year immutable cache;
the discovery document uses a short cache. This is a static file publication,
not a font service.

Set `MYSEKAI_FONT_CATALOG_URL` on this server to the operator's fixed HTTPS
`fonts/latest.json` (or an immutable revision URL). Redirects, credentials and
query strings are refused. The server validates the catalogue, checks its
immutable history, and caches it for 30 seconds. No editor request supplies a
fetch URL. The authenticated `/api/mysekai/font-coverage` endpoint exposes the
same coverage for early editor feedback.

Only MYSEKAI publication requires this configuration. Saving drafts, restoring
backups, rollback and unpublish remain available; other translation modules are
unaffected. Publication validates every current, source-matched nonempty draft
against supplemental font coverage before updating the public pointer. Missing
configuration, unavailable catalogue, or unsupported characters leave the old
public publication untouched. The editor's early check covers the currently
loaded draft; the server checks the entire publication.

The Moly stage discovers `fonts/latest.json` below its configured resource base
(or `/moly/` on the player origin). It uses the verified engine build's exact UI
font SHA to select base coverage. It computes the final displayed strings using
official/human/source precedence and source-line hashes, preserving literal
markup-like text. A font is downloaded only when those strings need code points
outside that exact base coverage. Official translations, human publications and
saved-draft previews use this same mechanism.

The first supplement is the approved full Resource Han Rounded font. It costs
29,350,000 raw bytes on first use; cached objects are reusable. A future operator
may publish smaller coverage shards in the same catalogue contract without
changing scene assets or requiring translators to do technical work. The current
SDK accepts up to eight 32 MiB fonts and prepares them within a bounded request.
Characters outside the approved font coverage remain explicitly unsupported.

Font hash/size checks complete before atomic text/font installation. A failed
download or missing glyph preserves the previous session and reports a playback
error. HTML reading success alone does not prove the stage rendered the text.
