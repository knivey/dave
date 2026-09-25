// gallery.js: IntersectionObserver infinite scroll over /gallery?after=
// fragments, DOM cap with scroll-up restore, pending-thumb retry
// (shimmer -> thumb swap), fragment-fetch retry with backoff, and SSE
// live updates (image-new prepend, thumb-ready swap, image-hidden card
// drop, reset reload).
"use strict";

import { connect } from "./sse.js";

const DOM_CAP = 200; // max cards attached to the DOM
const RETAIN_CAP = 400; // max detached cards kept in memory for restore
const SNIPPET_CHARS = 160; // mirrors clampSnippet / search.snippet_chars

// Module-scoped so the M5 filter hooks (setFilterActive /
// registerFilterClear / setFragmentURL / resetPaging / rewatchSentinel)
// can reach the grid, the paging state, and the pending batch from
// outside boot(). Populated by boot(); the hooks no-op before that.
let grid = null;
let pill = null;
let filterActive = false;
let filterClearFn = null;
let pendingNew = [];

// Detached (trimmed) gallery cards in their original grid order —
// newest first, exactly as they were attached. trim() moves cards here
// instead of discarding them so restore() can put them back with zero
// network traffic. Module-scoped (not boot-local) because search.js's
// filter-exit path replaces the grid wholesale and must drop the
// stale detached set (resetPaging) — restored gallery pages come from
// a fresh fetch, never from pre-filter trimmings.
let detached = [];

// fetch-URL provider for infinite scroll. Default fetches the live
// gallery fragment; search.js installs a query-scoped provider while
// results mode owns the grid (and restores the default on exit).
// Cursor is opaque to this module — each mode defines its own format.
let fragmentURLFn = null;

// Provider generation, bumped on every setFragmentURL swap. Retrying
// loadMore chains capture it so a swap can strand their pending retry
// timers (see setFragmentURL).
let providerGen = 0;

// Boot's observer.disconnect, hoisted for setFragmentURL (populated by
// boot(); null before that).
let disconnectObserverFn = null;

// Boot's watchSentinel, hoisted for the same outside-callers reason.
let watchSentinelImpl = null;

// setFilterActive tells the live-prepend path whether a search filter
// (M5) owns the grid. While active, arriving cards are buffered into
// the "+N new" pill instead of prepending — prepending into filtered
// results would lie about the filter. Clearing it flushes the batch.
export function setFilterActive(active) {
	filterActive = active;
	if (!active) flushPendingNew();
}

// registerFilterClear lets M5 attach its own clear action to the pill
// (reset the search box, restore the unfiltered grid). The pill defers
// to it entirely: the buffered batch flushes when the action calls
// setFilterActive(false) after a successful restore — a failed restore
// keeps the batch buffered behind the still-active filter.
export function registerFilterClear(fn) {
	filterClearFn = fn;
}

// setFragmentURL swaps the infinite-scroll fetch target (null restores
// the default /gallery fragment URL).
//
// Cross-mode cursor guard, applied when ENTERING a scoped mode (fn
// non-null): until the caller swaps the grid contents, the
// still-attached sentinel belongs to the PREVIOUS mode and carries
// that mode's cursor format. Letting it load now would pair the new
// provider's URL with the old cursor (e.g. /search-fragment?after=
// <gallery cursor> — a guaranteed 400 that only self-heals via
// backoff). So the swap disconnects the observer immediately (the
// grid swap's rewatchSentinel() re-arms it on the new sentinel) and
// bumps providerGen, which strands any pending retry timers still
// holding the old sentinel. The restore path (fn === null) skips the
// disconnect: it is already windowless — the grid is swapped BEFORE
// the provider is restored, synchronously, so no callback can run in
// between — and disconnecting there would kill the observer that the
// swap's rewatchSentinel() just armed on the fresh gallery sentinel.
// The generation bump still applies to both paths (stranding the
// superseded mode's retry chains is correct in either direction).
export function setFragmentURL(fn) {
	fragmentURLFn = fn;
	providerGen++;
	if (fn && disconnectObserverFn) disconnectObserverFn();
}

// resetPaging drops the detached-card set and any trim bookkeeping —
// called by search.js after replacing the grid contents, since cards
// detached from the previous mode's grid are no longer restorable.
export function resetPaging() {
	detached = [];
}

// rewatchSentinel re-arms the IntersectionObserver on whatever
// sentinel the grid currently contains (after search.js swapped the
// grid contents for a results page or a restored gallery page).
export function rewatchSentinel() {
	if (watchSentinelImpl) watchSentinelImpl();
}

function defaultFragmentURL(cursor) {
	return "/gallery?after=" + encodeURIComponent(cursor);
}

export function boot() {
	grid = document.getElementById("grid");
	if (!grid) return;

	const observer = new IntersectionObserver(
		(entries) => {
			for (const entry of entries) {
				if (entry.isIntersecting) loadMore(entry.target);
			}
		},
		{ rootMargin: "600px" }
	);

	let loading = false;
	let fetchFailures = 0;

	function watchSentinel() {
		// disconnect first: search.js's grid swaps remove the previous
		// sentinel from the DOM without loadMore ever unobserving it —
		// IntersectionObserver holds strong references, so skipping
		// this would leak one detached div per swap. Exactly one
		// sentinel exists at a time, so a full disconnect is safe.
		observer.disconnect();
		const sentinel = grid.querySelector(".sentinel");
		if (sentinel) observer.observe(sentinel);
	}

	function backoffMs() {
		if (fetchFailures <= 1) return 2000;
		if (fetchFailures === 2) return 5000;
		return 15000;
	}

	async function loadMore(sentinel) {
		const cursor = sentinel.dataset.nextCursor;
		if (!cursor || loading) return;
		const myGen = providerGen; // a provider swap strands this retry chain
		loading = true;
		try {
			const url = (fragmentURLFn || defaultFragmentURL)(cursor);
			const resp = await fetch(url);
			if (!resp.ok) throw new Error("HTTP " + resp.status);
			const doc = new DOMParser().parseFromString(await resp.text(), "text/html");
			// The fetch raced a grid swap (search.js entering/leaving
			// results mode replaces every child, including this
			// sentinel). A detached sentinel means this page belongs to
			// the previous mode's grid — dropping it is exactly right.
			if (!sentinel.isConnected) return;
			for (const card of doc.querySelectorAll("article.card")) {
				grid.appendChild(card);
			}
			observer.unobserve(sentinel);
			sentinel.remove();
			trim();
			watchSentinel();
			fetchFailures = 0; // success resets the retry schedule
		} catch (err) {
			// A failed fragment fetch must not silently kill infinite
			// scroll: the IntersectionObserver will not refire while the
			// still-intersecting sentinel's state is unchanged, so
			// schedule our own retry with backoff. Retrying stops when a
			// load succeeds (sentinel replaced, failures reset), when a
			// provider swap supersedes this mode (myGen mismatch), or
			// when the last page removed the sentinel entirely.
			fetchFailures++;
			console.warn("gallery fragment fetch failed; retrying in " + backoffMs() + "ms", err);
			setTimeout(() => {
				if (sentinel.isConnected && myGen === providerGen) loadMore(sentinel);
			}, backoffMs());
		} finally {
			loading = false;
		}
	}

	// trim detaches the cards at the TOP of the grid — the newest ones,
	// i.e. the side a downward-scrolling viewport is moving away from —
	// once DOM_CAP is exceeded. Detached cards are retained (up to
	// RETAIN_CAP) for restore(); past the retention cap the
	// oldest-detached cards (the array tail) are dropped for good.
	// Downward paging state lives solely on the bottom sentinel's
	// data-next-cursor, and detached cards sit above whatever the
	// sentinel tracks, so reattaching them never disturbs the
	// infinite-scroll cursor bookkeeping.
	function trim() {
		const cards = grid.querySelectorAll("article.card");
		const over = cards.length - DOM_CAP;
		if (over <= 0) return;
		const batch = Array.from(cards).slice(0, over);
		for (const card of batch) card.remove();
		detached = batch.concat(detached);
		if (detached.length > RETAIN_CAP) {
			detached.length = RETAIN_CAP;
		}
	}

	// restore reattaches every retained card once the user scrolls back
	// to the top, prepending them in order (oldest-detached first, so the
	// grid ends up newest-first again). The scroll offset is shifted by
	// the height the prepend added, keeping the currently visible cards
	// in view instead of jumping to the new top. Skipped while a search
	// filter owns the grid: reattaching live-gallery cards into filtered
	// results would lie about the filter (the same rule as live
	// prepends); search.js's exit path refetches the gallery instead.
	//
	// Results-mode asymmetry (accepted): trim() is NOT filter-gated, so
	// in results mode the top-ranked cards still detach once the grid
	// passes DOM_CAP — and with restore() gated off, scrolling back up
	// cannot recover them; they stay detached until the next query
	// swap's resetPaging drops them. Acceptable at realistic query
	// sizes: trimming only begins past 200 attached result cards.
	function restore() {
		if (filterActive || detached.length === 0) return;
		const doc = document.documentElement;
		const prevHeight = doc.scrollHeight;
		const prevTop = window.scrollY;
		const frag = document.createDocumentFragment();
		for (const card of detached) frag.appendChild(card);
		grid.prepend(frag);
		detached = [];
		// Re-arm pending thumbs whose retry timer was dropped while the
		// card was detached (the timer guard skips disconnected imgs):
		// without this a restored card would shimmer until a thumb-ready
		// event happened to arrive while it is attached again.
		for (const img of grid.querySelectorAll("img.pending")) {
			if (!img.getAttribute("src") && img.dataset.thumb) img.src = img.dataset.thumb;
		}
		window.scrollTo(0, prevTop + (doc.scrollHeight - prevHeight));
	}

	// Scroll-up restore. Passive: the handler never needs to prevent
	// scrolling (the scrollTo compensation runs after layout).
	window.addEventListener(
		"scroll",
		() => {
			if (window.scrollY <= 600) restore();
		},
		{ passive: true }
	);

	// Pending thumbs 404 (no-cache) until the worker flips them ready.
	// 'error' does not bubble, so listen in capture phase. Cards that
	// carry a data-orig fallback (server-rendered) retry once and then
	// switch to the original bytes. Cards WITHOUT one (SSE-prepended:
	// the image-new payload has no filename, so the orig URL can't be
	// built client-side) have no better end state than "still waiting",
	// so they STAY .pending and keep retrying with capped exponential
	// backoff. Dropping the pending class here (the old behavior) left
	// a dead near-black box (the card img's #0d0d0f background) that
	// the later thumb-ready swap — which targets img.pending — could
	// never heal; generation regularly outlasts the first retry window.
	//
	// While waiting between attempts the src attribute is removed and
	// the intended thumb URL stashed in data-thumb: a src-less
	// .pending img renders as the pure shimmer placeholder (no
	// broken-image glyph), and the stash lets both the retry timer and
	// the thumb-ready swap restore the fetch.
	//
	// Retry base is 2s: the thumb-ready SSE event — which heals the
	// card the instant the worker finishes — is the PRIMARY mechanism,
	// and retries exist only as the fallback for a missed event. The
	// hosting box resizes slowly (Intel Atoms), so a 2s first attempt
	// avoids refetching a worker that is provably still grinding while
	// keeping the fallback responsive.
	grid.addEventListener(
		"error",
		(e) => {
			const img = e.target;
			if (!(img instanceof HTMLImageElement) || !img.classList.contains("pending")) return;
			const retries = Number(img.dataset.retries || 0);
			if (img.dataset.orig && retries >= 1) {
				// One retry already failed: fall back to the original bytes.
				img.classList.remove("pending");
				img.src = img.dataset.orig;
				return;
			}
			img.dataset.retries = String(retries + 1);
			if (!img.dataset.thumb) img.dataset.thumb = img.src;
			img.removeAttribute("src"); // clean shimmer while waiting
			setTimeout(() => {
				if (img.classList.contains("pending") && img.isConnected) {
					img.src = img.dataset.thumb; // no-cache 404s refetch
				}
			}, Math.min(2000 * 2 ** retries, 30000));
		},
		true
	);

	// A successful fetch of any kind (initial load, retry, thumb-ready
	// swap) clears the shimmer. 'load' does not bubble either, so this
	// also captures. Without it, a card whose thumb arrived between
	// retries — or a replayed image-new for an image that was already
	// ready — kept the shimmer class forever over a loaded image.
	grid.addEventListener(
		"load",
		(e) => {
			const img = e.target;
			if (img instanceof HTMLImageElement && img.classList.contains("pending")) {
				img.classList.remove("pending");
			}
		},
		true
	);

	// Localize timestamps (server text stays as the no-JS fallback).
	for (const t of grid.querySelectorAll("time[data-ts]")) {
		try {
			t.textContent = new Date(t.dataset.ts).toLocaleString();
		} catch {
			/* keep server-rendered text */
		}
	}

	watchSentinelImpl = watchSentinel;
	disconnectObserverFn = () => observer.disconnect();
	setupLiveUpdates();
	watchSentinel();
}

// --- SSE live updates (milestone 4) ---

// clampPrompt mirrors the server's clampSnippet (rune count including
// the ellipsis) so SSE-prepended cards match fragment-rendered ones.
function clampPrompt(s) {
	if (!s) return "";
	const r = Array.from(s);
	if (r.length <= SNIPPET_CHARS) return s;
	return r.slice(0, SNIPPET_CHARS - 1).join("") + "…";
}

// buildCard mirrors the server's "cards" fragment markup (pages.go):
// same classes and data hooks (data-id, data-ts, .pending shimmer) so
// thumb-ready swaps and error handling treat prepended and rendered
// cards identically. The SSE payload carries no filename, so — unlike
// server cards — no data-orig fallback URL can be attached; see the
// error listener above for how that degrades.
function buildCard(ev) {
	const article = document.createElement("article");
	article.className = "card";
	article.dataset.id = ev.id;

	const link = document.createElement("a");
	link.className = "card-link";
	link.href = ev.page_url || "/" + ev.id;

	const img = document.createElement("img");
	img.className = "pending"; // thumb_status is "pending" on arrival
	img.loading = "lazy";
	img.src = "/" + ev.id + "/t/small";
	img.alt = clampPrompt(ev.original_prompt);
	link.appendChild(img);

	const prompt = document.createElement("p");
	prompt.className = "prompt";
	prompt.textContent = clampPrompt(ev.original_prompt);

	const time = document.createElement("time");
	const ts = new Date(ev.created_at);
	time.dateTime = ev.created_at;
	time.dataset.ts = ev.created_at;
	time.textContent = isNaN(ts.getTime()) ? ev.created_at : ts.toLocaleString();

	article.append(link, prompt, time);
	return article;
}

function prependCard(ev) {
	if (!grid) return;
	// Dedup covers the live DOM AND the detached set. Live DOM: replay
	// after visibility-reopen can redeliver an event whose card is
	// attached. Detached set: since=0 full-ring replays (the
	// zero-event reopen path) re-deliver image-new for images whose
	// cards were fragment-rendered and later trimmed — prepending a
	// fresh twin while the original sits detached would duplicate the
	// card on restore(). Dropping the detached twin (the fresh prepend
	// carries the same data) matches onImageHidden's sweep pattern.
	if (grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]')) return;
	for (let i = detached.length - 1; i >= 0; i--) {
		if (detached[i].dataset.id === ev.id) detached.splice(i, 1);
	}
	grid.prepend(buildCard(ev));
	// Deliberately NOT trim()-ming here: trim detaches the TOP cards,
	// which during a live prepend are exactly the ones the user is
	// watching. DOM growth is bounded by attention — arrivals only
	// deliver while the tab is visible (sse.js closes hidden streams),
	// and the scroll path re-imposes DOM_CAP on the next page fetch.
}

function flushPendingNew() {
	if (pendingNew.length === 0) return;
	const batch = pendingNew;
	pendingNew = [];
	for (const ev of batch) prependCard(ev);
	updatePill();
}

function updatePill() {
	if (!pill) return;
	pill.hidden = pendingNew.length === 0;
	pill.textContent = "+" + pendingNew.length + " new";
}

function onImageNew(ev) {
	if (!ev || !ev.id) return;
	if (filterActive) {
		// A filter owns the grid: buffer arrivals so the filtered view
		// never lies, and surface them behind the "+N new" pill.
		pendingNew.push(ev);
		updatePill();
		return;
	}
	prependCard(ev);
}

// onThumbReady heals a live card's shimmer when the worker publishes
// thumb-ready.
//
// DESIGN NOTE — heal invariant: the `pending` class is removed ONLY by a
// successful `load` (the capture-phase load listener) or by the data-orig
// terminal fallback in the error listener — NEVER here. The old code did
// `classList.remove("pending"); img.src = img.dataset.thumb || img.src;`
// and that pair was a production killer (watched tab, slow resize host,
// Sep 2026): when a retry's 404 fetch was still IN FLIGHT the src
// attribute already equaled the target, and assigning an unchanged src is
// a verified no-op in Chromium (no refetch, no abort — the in-flight 404
// kept ownership of the element). The 404 then landed on a non-pending
// img, the error listener early-returned on its pending guard, and the
// card died: failed-load state, no retries armed, shimmer gone (near-black
// box) — while the server thumb WAS ready.
//
// So instead: FORCE a real refetch and let load/error resolve everything,
// with pending deliberately retained. Mechanism — cache-busting query
// (`?v=<ms>`): empirically verified in Chromium (playwright-core probe,
// ~/dev/imgsite-debug/probe-restart.ts) as the ONLY one of the two
// candidates that restarts the load when the attribute already equals the
// target URL. The alternative — removeAttribute("src") then re-assign in
// the same task — is a verified no-op (the final attribute value is
// unchanged, so Chromium's image-loading update dedupes it; probe case
// P1). Path routes ignore query strings, and pending 404s are no-cache,
// so the bust's only cost is one extra immutable cache entry per race
// event.
//
// Self-healing in BOTH orders (this is the exact race that killed the old
// code):
//   - forced fetch 200s -> the load listener clears pending (the ONLY
//     pending-clearing path). A stale error from the superseded in-flight
//     404, if the engine surfaces one at all, lands on a non-pending img
//     and the error listener early-returns — harmless.
//   - forced fetch 404s anyway (event raced the worker flip, or a proxy
//     served stale) -> pending is still set, so the error listener sees a
//     retryable pending img and re-arms the capped backoff chain; the
//     next no-cache retry lands the ready thumb and the load listener
//     clears pending then.
function onThumbReady(ev) {
	if (!grid || !ev || !ev.id) return;
	const card = grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]');
	if (!card) return; // trimmed from DOM, or another page state
	const img = card.querySelector("img.pending");
	if (!img) return; // already swapped or already failed
	// Capture the target BEFORE mutating: mid-retry-wait imgs have no src
	// attribute at all (the error listener removed it for a clean
	// shimmer), so the stashed data-thumb is the source of truth there.
	const target = img.dataset.thumb || img.getAttribute("src");
	if (!target) return;
	img.src = target + (target.includes("?") ? "&" : "?") + "v=" + Date.now();
}

// onImageHidden drops a soft-deleted image's card (SSE image-hidden,
// published by DELETE /api/images/<id> after the row flips hidden —
// milestone 7). This handler serves BOTH the live gallery and search
// results (the search page boots the same data-page="gallery" module
// set, so its SSE wiring is this exact connect() call). The detached
// set is swept too: restore() reattaches trimmed cards with zero
// network traffic, so one left behind would resurrect the deleted
// image until the next full reload.
function onImageHidden(ev) {
	if (!grid || !ev || !ev.id) return;
	const card = grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]');
	if (card) card.remove();
	for (let i = detached.length - 1; i >= 0; i--) {
		if (detached[i].dataset.id === ev.id) detached.splice(i, 1);
	}
}

function setupLiveUpdates() {
	// "+N new" pill: offered only while a filter is holding arrivals
	// back. Clicking defers entirely to the registered clear action
	// (M5): it restores the gallery and calls setFilterActive(false)
	// on success, whose flush prepends the batch into the LIVE grid.
	// If the restore fetch fails, the filter stays active and the
	// batch stays buffered — flushing here would prepend arrivals into
	// the stale results grid, lying about the filter. With no
	// registered action (unfiltered mode poked via the console hooks)
	// there is nothing to restore, so clear + flush directly.
	pill = document.createElement("button");
	pill.id = "new-pill";
	pill.type = "button";
	pill.hidden = true;
	document.body.appendChild(pill);
	pill.addEventListener("click", () => {
		if (filterClearFn) {
			filterClearFn(); // flush happens in its setFilterActive(false)
			return;
		}
		setFilterActive(false); // no clear action: flush directly
	});

	// Window-level handle on the filter hooks: M5's search.js imports
	// this module directly, but the hook is also pokable from the
	// console for debugging.
	window.imgsite = window.imgsite || {};
	window.imgsite.gallery = { setFilterActive, registerFilterClear };

	// First-connect replay cursor: the server embedded the event id it
	// rendered this page's snapshot at (body[data-last-event], present
	// whenever a hub exists — 0 is meaningful). connect() turns it into
	// ?since=<id> on the FIRST open, replaying the render→subscribe gap
	// (an upload committed after the page's DB query but before the
	// EventSource connected); prependCard's data-id dedup absorbs the
	// harmless overlap of an image that made it into both the page and
	// the replay. On this page that covers both entry templates — the
	// gallery and the server-rendered /search?q= results page (whose
	// replayed arrivals buffer behind the pill like any live arrival
	// during a filter). Absent attribute → undefined → live-only, the
	// nil-hub / no-cursor fallback.
	connect(
		{
			"image-new": onImageNew,
			"thumb-ready": onThumbReady,
			"image-hidden": onImageHidden,
			// Overflow / replay-gap recovery: full refetch is always correct.
			onReset: () => location.reload(),
		},
		{ since: document.body.dataset.lastEvent }
	);
}
