# Backlog

Small owner-queued items, not yet scheduled:

- **imgsite results-mode trim asymmetry is now live** (surfaced by the
  Sep 26 paging repair): `trim()` is NOT filter-gated while
  `restore()` is, so results-mode walks past 200 attached cards (first
  time reachable — paging used to stall at 96) detach top-ranked cards
  with no scroll-back recovery until the next query swap. Needs a small
  design pass: enable restore in results mode (investigate why it was
  filter-gated — pill/pendingNew interplay) or gate trim off in
  results mode (unbounded DOM growth instead). See the asymmetry
  comment block in `imgsite/web/gallery.js` (~243) — its "not
  reachable at realistic query sizes" premise died with the paging
  fix. Reviewer nits from the same round, fix alongside: `baseBox()`
  stale on mid-gesture rotation (image.js), harness gaps (unzoomed
  touch-drag no-close, pointercancel coverage, scroll-anchor
  comparison in restore check).
- **img-mcp logging polish**: DONE Sep 26 2026 (`ada1373`) — kept for
  history; see git.

Parked elsewhere: backfill tooling for pre-safe-site images (EXIF
provenance backfill, batch LLM vetting) — see the Deferred section of
`docs/superpowers/specs/2026-09-26-safe-site-design.md`.
