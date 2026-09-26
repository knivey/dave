# Backlog

Small owner-queued items, not yet scheduled:

- **imgsite details page `<title>`**: use the user prompt (original,
  fallback to enhanced when empty), truncated — instead of the current
  static/generic title. Improves browser-tab identification and
  bookmark/History labels. (Queued Sep 26 2026, mid safe-site plan.)
- **safe-site deferred minors** (final review triage Sep 26 2026: none
  block anything; fix opportunistically):
  - imgsite: defensive comment for hand-built Safe&&empty siteCtx;
    ToLower-unicode vs LOWER-ASCII parity note (fails closed);
    HiddenIs410 test non-discriminating (use hidden-efnet row);
    sit0001 fixture implicit libera default; default-host SQL-text
    identity pinned only for tier 3; route-level hidden+invisible 410
    test; safe-host asset matrix exercises network branch only;
    narrow thumb-ready re-read SELECT to needed columns; spurious
    extraction WARN before skip-network check on img-mcp recovery;
    README upload.* vs docs upload.rate_per_minute wording.
  - img-mcp: safetyVet.wait() bounded only by enhancement timeout
    (trust-the-config); unlocked vet-missing WARN sink swap in tests;
    exifTagModel unused-in-prod constant; TIFF word-alignment/sub-IFD
    exotic-input notes; multi-image EXIF rewrite untested;
    AGENTS jobs.safety parenthetical + hash-ordering wording
    tightenings.
