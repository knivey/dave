// gallery.js: IntersectionObserver infinite scroll over /gallery?after=
// fragments, DOM cap with scroll-up restore, pending-thumb placeholder
// (dark bytes -> thumb swap on thumb-ready; retry is fallback-only for
// genuine failures), fragment-fetch retry with backoff, top-rated
// (net-score) sort mode (fragment URLs carry &sort=liked, arrivals
// buffer behind the pill), and SSE live updates (image-new prepend,
// thumb-ready swap, image-hidden card drop, image-liked tally
// updates, image-reacted badge rebuilds, reset reload).
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

// Detached (trimmed) cards in their original grid order — exactly as
// they were attached. trim() moves cards here instead of discarding
// them so restore() can put them back with zero network traffic.
// Module-scoped (not boot-local) because search.js's grid swaps must
// drop the stale detached set (resetPaging) on BOTH edges — entering a
// query AND restoring the gallery. That is the invariant which lets
// restore() stay mode-agnostic: anything still in here was trimmed
// from the grid currently attached.
let detached = [];

// fetch-URL provider for infinite scroll. Default fetches the live
// gallery fragment; search.js installs a query-scoped provider while
// results mode owns the grid (and restores the default on exit).
// Cursor is opaque to this module — each mode defines its own format.
let fragmentURLFn = null;

// Top-rated sort mode (body[data-sort="liked"], set by the server on
// /?sort=liked). The rank is the NET score (likes − dislikes — see
// db.go), but the URL/attr keep the historical "liked" name. Three
// load-bearing consequences:
//   - fetched fragments must carry &sort=liked (defaultFragmentURL);
//   - live image-new arrivals buffer behind the "+N new" pill —
//     prepending into a count-sorted grid would lie about the order —
//     and the pill RELOADS instead of flushing (see its click
//     handler);
//   - a card whose count rose mid-scroll can reappear on a fetched
//     page (the mutable-sort-cursor caveat, db.go) — loadMore's
//     append-dedupe absorbs it.
let sortLiked = false;

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

// galleryQS returns the live gallery's sort query string — "" or
// "sort=liked" — as the ONE source of truth every path that
// re-fetches or re-addresses the gallery derives from:
// defaultFragmentURL (infinite scroll), and search.js's
// restoreGallery (its swapGrid fetch + history.replaceState). A path
// that hardcodes "/" or "/gallery" instead stranding a mode mismatch:
// the restored grid would carry default-mode cursors while
// defaultFragmentURL still appends &sort=liked — every subsequent
// page fetch 400s on the arity check and the retry chain re-400s
// forever. Set by boot() from body[data-sort].
export function galleryQS() {
	return sortLiked ? "sort=liked" : "";
}

function defaultFragmentURL(cursor) {
	const qs = galleryQS();
	const base = "/gallery?after=" + encodeURIComponent(cursor);
	return qs ? base + "&" + qs : base;
}

export function boot() {
	grid = document.getElementById("grid");
	if (!grid) return;
	sortLiked = document.body.dataset.sort === "liked";

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
			// Append-dedupe by data-id: a no-op in default mode, but
			// the liked sort keys on a MUTABLE column (net score), so
			// a row whose count rose between page fetches can legally
			// reappear on the next page (see db.go's mutable-sort
			// note). Skipping an already-attached card keeps the grid
			// honest without punishing default mode. The set is seeded
			// from the DETACHED cards too: an un-like can drop a
			// trimmed top card's key back below the cursor, and its
			// reappearance on a fetched page would append a twin while
			// the original waits in `detached` — restore() would then
			// prepend a duplicate data-id. Skipping it here keeps the
			// detached original the single copy (it reattaches on
			// scroll-back with its live like-count span intact).
			const seen = new Set(
				Array.from(grid.querySelectorAll("article.card")).map((c) => c.dataset.id)
			);
			for (const d of detached) seen.add(d.dataset.id);
			for (const card of doc.querySelectorAll("article.card")) {
				if (seen.has(card.dataset.id)) continue;
				seen.add(card.dataset.id);
				grid.appendChild(card);
			}
			// The fetched page carries its own successor sentinel: the
			// "cards" partial emits a fresh <div class="sentinel"
			// data-next-cursor=…> AFTER the card range whenever more
			// pages exist. Append it BEFORE dropping the old sentinel so
			// watchSentinel() below has something to re-arm on. Skipping
			// this (the old behavior) disarmed the observer for good on
			// the FIRST fetched page: attached cards capped at ~96 —
			// initial page + one fragment — which sits under DOM_CAP, so
			// trim()/restore() were unreachable and long scrolls
			// stalled mid-gallery while the server still had rows.
			const next = doc.querySelector(".sentinel");
			if (next) grid.appendChild(next);
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
	//
	// ORDERING (paging-repair finding, verified in a real browser):
	// batches accumulate OLDEST-trimmed-first → NEW batches are
	// CONCATENATED AT THE TAIL. trim() always slices from the top of
	// the remaining grid, so each successive batch is strictly OLDER
	// than every card already in `detached`; appending at the tail
	// keeps the array newest-first, which is exactly the order
	// restore() must prepend in (see below). The previous
	// `batch.concat(detached)` built the array oldest-batch-first —
	// unreachable while the sentinel bug kept trim() dead, it
	// reattached cards as [B2, B1, rest] instead of [B1, B2, rest],
	// scrambling gallery order after any deep scroll + scroll-back.
	function trim() {
		const cards = grid.querySelectorAll("article.card");
		const over = cards.length - DOM_CAP;
		if (over <= 0) return;
		const batch = Array.from(cards).slice(0, over);
		for (const card of batch) card.remove();
		detached = detached.concat(batch);
		if (detached.length > RETAIN_CAP) {
			detached.length = RETAIN_CAP;
		}
	}

	// restore reattaches every retained card once the user scrolls back
	// to the top, prepending them in array order — first-trimmed batch
	// first, so the grid ends up in the exact order the pages were
	// attached in (newest-first in the gallery, rank-first in search
	// results; see trim()'s ORDERING note). The scroll offset is
	// shifted by the height the prepend added, keeping the currently
	// visible cards in view instead of jumping to the new top.
	//
	// Mode-agnostic by construction: trim()/restore() run in EVERY
	// mode (default, search results, liked sort). The mode guard is
	// resetPaging at swapGrid's EDGES (search.js) — it drops the whole
	// detached set both when a query takes over the grid and when the
	// gallery is restored — so anything still in `detached` was trimmed
	// from the grid CURRENTLY attached, and reattaching it can never
	// leak one mode's cards into another's grid. (swapGrid runs
	// resetPaging and replaceChildren back-to-back, synchronously; the
	// only async window, between enterResultsMode and the fetch
	// resolving, still shows the old grid — matching the old detached
	// set — so a restore firing there is also correct.) A filterActive
	// gate here used to strand results-mode trims instead: top-ranked
	// cards past DOM_CAP could never be recovered by scrolling back up
	// (the old "results-mode asymmetry", since closed).
	//
	// Liked-mode wrinkle (accepted): restored cards sit at their
	// detach-time position, which concurrent likes may have made stale
	// — the same mutable-count caveat loadMore's append-dedupe
	// documents (net score is a moving sort key, db.go). The counts
	// themselves stay correct either way: onImageLiked sweeps the
	// detached set while the cards wait here. The duplicate shape the
	// same mutability could otherwise produce — an un-like dropping a
	// detached card's key below the cursor so a fetched page re-serves
	// it — is closed by loadMore seeding its dedupe set from
	// `detached`.
	function restore() {
		if (detached.length === 0) return;
		const doc = document.documentElement;
		const prevHeight = doc.scrollHeight;
		const prevTop = window.scrollY;
		const frag = document.createDocumentFragment();
		for (const card of detached) frag.appendChild(card);
		grid.prepend(frag);
		detached = [];
		// Re-arm imgs the thumb pipeline left in the src-less shimmer
		// state while the card was detached (the timer guard skips
		// disconnected imgs): without this a restored card would
		// shimmer until a thumb-ready event happened to arrive while
		// it is attached again. Two producers of that shape now: the
		// error listener's retry wait, and onThumbReady's detached
		// sweep, which pre-stages exactly this state so restore()
		// finishes the heal (refetch while attached; the load listener
		// clears the shimmer).
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

	// Pending thumbs serve 200 with placeholder JPEG bytes
	// (Cache-Control: no-store) until the worker flips them ready, so
	// the normal pending path never errors at all — the img loads the
	// placeholder and the thumb-ready swap replaces it. This error
	// listener is therefore FALLBACK-ONLY for genuine failures: a
	// failed row (404, no-cache), ready-row/store drift, or proxy
	// trouble. 'error' does not bubble, so listen in capture phase.
	// Cards that carry a data-orig fallback (server-rendered) retry
	// once and then switch to the original bytes. Cards WITHOUT one
	// (SSE-prepended: the image-new payload has no filename, so the
	// orig URL can't be built client-side) have no better end state
	// than "still waiting", so they STAY .pending and keep retrying
	// with capped exponential backoff. Dropping the pending class
	// here (the old behavior) left a dead near-black box (the card
	// img's #0d0d0f background) that the later thumb-ready swap could
	// never heal; generation regularly outlasts the first retry
	// window.
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

	// A successful fetch of any kind (placeholder load, real thumb,
	// retry, thumb-ready swap) clears the shimmer. 'load' does not
	// bubble either, so this also captures. Without it, a card whose
	// thumb arrived between retries — or a replayed image-new for an
	// image that was already ready — kept the shimmer class forever
	// over a loaded image. NOTE: this fires on the PLACEHOLDER load
	// too — a pending card loses .pending within milliseconds of
	// arrival. That is exactly why onThumbReady must target the card's
	// img by data-id instead of img.pending (see its DESIGN NOTE).
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
// cards identically. With pending thumbs serving placeholder bytes the
// .pending shimmer lasts only until that load completes; the
// thumb-ready swap then replaces the placeholder. The SSE payload
// carries no filename, so — unlike server cards — no data-orig
// fallback URL can be attached; see the error listener above for how
// that degrades on genuine failure.
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

	// Meta row mirrors the server card shape (time + optional .counts
	// wrapper inside div.meta) so the image-liked/image-reacted
	// updaters can treat prepended and rendered cards identically.
	// New arrivals have zero votes and reactions, so no counts spans
	// are created here.
	const meta = document.createElement("div");
	meta.className = "meta";
	meta.appendChild(time);

	article.append(link, prompt, meta);
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
	// A filter or the top-rated sort owns the grid: buffer arrivals so
	// the ordered view never lies. (Liked mode's pill click reloads —
	// prepending would violate the count sort; see its handler.)
	if (filterActive || sortLiked) {
		pendingNew.push(ev);
		updatePill();
		return;
	}
	prependCard(ev);
}

// onThumbReady heals a live card's placeholder (or shimmer) when the
// worker publishes thumb-ready.
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
// DESIGN NOTE — target by card, not by .pending: pending thumbs now
// serve 200 placeholder bytes, and that successful load clears
// .pending within milliseconds of the card's arrival — by the time
// thumb-ready fires, the img is virtually never .pending anymore.
// Querying img.pending here (the pre-placeholder shape) would
// early-return and strand the placeholder forever. The card's img is
// targeted by the card's data-id instead, unconditionally.
//
// So instead: FORCE a real refetch and let load/error resolve everything,
// with the class left to the sanctioned clearers. Mechanism —
// cache-busting query (`?v=<ms>`): empirically verified in Chromium
// (playwright-core probe, ~/dev/imgsite-debug/probe-restart.ts) as the
// ONLY one of the two candidates that restarts the load when the
// attribute already equals the target URL. The alternative —
// removeAttribute("src") then re-assign in the same task — is a
// verified no-op (the final attribute value is unchanged, so
// Chromium's image-loading update dedupes it; probe case P1). Path
// routes ignore query strings, ready responses are immutable, and the
// placeholder is no-store, so the bust's only cost is one extra
// immutable cache entry per race event. If the img already shows the
// real thumb (a retry landed first), the bust just re-loads the same
// ready bytes — harmless.
//
// Self-healing in BOTH orders (this is the exact race that killed the old
// code):
//   - forced fetch 200s -> the load listener clears pending if it was
//     still set (the ONLY pending-clearing path). A stale error from the
//     superseded in-flight 404, if the engine surfaces one at all, lands
//     on a non-pending img and the error listener early-returns —
//     harmless.
//   - forced fetch 404s anyway (genuine failure/drift) -> the error
//     listener sees a retryable img if it is still pending and re-arms
//     the capped backoff chain; the next no-cache retry decides the
//     end state then.
//
// DESIGN NOTE — detached-card sweep: a card trimmed out of the grid by
// trim() into the detached set is invisible to the live swap above
// (grid.querySelector cannot reach it), and pre-placeholder it was
// still healed eventually because restore()'s re-arm matched the
// mid-retry shape (shimmer + no src + stashed data-thumb). The
// placeholder broke that: its successful load cleared the shimmer and
// left src set, so a trimmed card kept STALE PLACEHOLDER BYTES until a
// full reload. The sweep below restores healability the same way
// onImageHidden sweeps the set: pre-stage the exact state restore()'s
// re-arm finishes — refresh the data-thumb stash, remove src, re-present
// the shimmer class. Assigning src right here instead would be wrong:
// a load fired on a detached img never reaches the grid's capture-phase
// listeners (events do not propagate from outside the subtree), so the
// sanctioned pending-clearer could not run and the bookkeeping above
// would silently desync. Pre-staged + restored, the refetch loads while
// ATTACHED, the load listener clears the shimmer, and the whole
// invariant holds unchanged.
function onThumbReady(ev) {
	if (!grid || !ev || !ev.id) return;
	const card = grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]');
	if (card) {
		// Live swap. Capture the target BEFORE mutating: mid-retry-wait
		// imgs have no src attribute at all (the error listener removed
		// it for a clean shimmer), so the stashed data-thumb is the
		// source of truth there.
		const img = card.querySelector("img");
		if (img) {
			const target = img.dataset.thumb || img.getAttribute("src");
			if (target) {
				img.src = target + (target.includes("?") ? "&" : "?") + "v=" + Date.now();
			}
		}
	}
	// Detached sweep (see DESIGN NOTE above): pre-stage the state
	// restore()'s re-arm finishes. Removing src is required, not
	// cosmetic — the re-arm only picks imgs with NO src attribute.
	for (let i = detached.length - 1; i >= 0; i--) {
		if (detached[i].dataset.id !== ev.id) continue;
		const img = detached[i].querySelector("img");
		if (!img) continue;
		const target = img.dataset.thumb || img.getAttribute("src");
		if (!target) continue;
		img.dataset.thumb = target;
		img.removeAttribute("src");
		img.classList.add("pending");
	}
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

// setCardCounts syncs one card's .likes/.dislikes spans (inside the
// .counts wrapper) with the tallies: each span created when its count
// first moves past zero, textContent swapped while it stays positive,
// removed when it falls back to zero — the same
// present-only-when-nonzero contract the server template renders. The
// wrapper itself is created on the first non-zero tally of any kind
// (vote or reaction) and dropped only when BOTH vote tallies and the
// reaction strip are gone, exactly mirroring the template's
// {{if or .LikeCount .DislikeCount .TopReactions}} guard.
function setCardCounts(card, likes, dislikes) {
	let wrap = card.querySelector(".counts");
	if (likes <= 0 && dislikes <= 0) {
		// Votes fell to zero: the SPANS must go even when the wrapper
		// survives to host a reaction strip — skipping this (the
		// pre-reactions shape, where wrap.remove() took the spans
		// along) stranded a stale "♥ 1" next to live badges forever.
		if (wrap) {
			setTallySpan(wrap, "likes", "\u2665", 0);
			setTallySpan(wrap, "dislikes", "\uD83D\uDC94", 0);
			if (!wrap.querySelector(".reacts")) wrap.remove();
		}
		return;
	}
	if (!wrap) {
		wrap = document.createElement("span");
		wrap.className = "counts";
		const meta = card.querySelector(".meta");
		if (!meta) return; // malformed card: nothing to hang the wrapper on
		meta.appendChild(wrap);
	}
	setTallySpan(wrap, "likes", "\u2665", likes);
	setTallySpan(wrap, "dislikes", "\uD83D\uDC94", dislikes);
}

// setTallySpan manages ONE tally span inside the .counts wrapper:
// create-on-first-nonzero, textContent swap, remove-on-zero.
function setTallySpan(wrap, className, glyph, count) {
	let span = wrap.querySelector("." + className);
	if (count > 0) {
		if (!span) {
			span = document.createElement("span");
			span.className = className;
			wrap.appendChild(span);
		}
		span.dataset.count = String(count);
		span.textContent = glyph + " " + count;
	} else if (span) {
		span.remove();
	}
}

// topBadges picks the card's badge list from a tally map whose keys
// ARE the emoji (rendered as their own glyphs — any emoji can be
// reacted, there is no configured whitelist): top 4 by count
// (cardReactionBadges), ties broken codepoint-lexicographically
// (emojiKeyLess — server parity with Go's emojiLess), plus whether
// more non-zero keys did not fit — the caller renders a "…"
// overflow marker. Mirrors topCardReactions server-side.
function topBadges(tally) {
	if (!tally) return { list: [], more: false };
	const all = [];
	for (const emoji of Object.keys(tally)) {
		const n = tally[emoji];
		if (Number.isFinite(n) && n > 0) {
			all.push({ emoji, count: n });
		}
	}
	all.sort((a, b) => (b.count - a.count) || (emojiKeyLess(a.emoji, b.emoji) ? -1 : 1));
	return { list: all.slice(0, cardReactionBadges), more: all.length > cardReactionBadges };
}

// emojiKeyLess: codepoint-lexicographic compare (Array.from iterates
// code points, not UTF-16 units) — matches Go's emojiLess so badge
// order is identical on both sides.
function emojiKeyLess(a, b) {
	const ca = Array.from(a), cb = Array.from(b);
	const n = Math.min(ca.length, cb.length);
	for (let i = 0; i < n; i++) {
		if (ca[i] !== cb[i]) return ca[i] < cb[i];
	}
	return ca.length < cb.length;
}

// cardReactionBadges mirrors the server's constant (reactions.go):
// how many reaction badges a card shows before the "…" marker.
const cardReactionBadges = 4;

// setCardReactions rebuilds one card's .reacts strip (inside the
// .counts wrapper, after the vote spans) from a tally map whose keys
// ARE the emoji (rendered directly — no glyph lookup). Rebuild-not-
// diff: the top-4 membership and the overflow marker can change on
// any toggle, so replacing the strip's children is the simple
// correct move. The strip drops when no configured emoji has a
// count; the wrapper drops with it only when the vote spans are gone
// too (shared lifecycle with setCardCounts — see its comment).
function setCardReactions(card, tally) {
	let wrap = card.querySelector(".counts");
	const { list: badges, more } = topBadges(tally);
	if (badges.length === 0 && !more) {
		if (wrap) {
			const strip = wrap.querySelector(".reacts");
			if (strip) strip.remove();
			if (!wrap.querySelector(".likes") && !wrap.querySelector(".dislikes")) wrap.remove();
		}
		return;
	}
	if (!wrap) {
		wrap = document.createElement("span");
		wrap.className = "counts";
		const meta = card.querySelector(".meta");
		if (!meta) return; // malformed card: nothing to hang the wrapper on
		meta.appendChild(wrap);
	}
	let strip = wrap.querySelector(".reacts");
	if (!strip) {
		strip = document.createElement("span");
		strip.className = "reacts";
		wrap.appendChild(strip);
	}
	const parts = [];
	for (const b of badges) {
		const s = document.createElement("span");
		s.className = "react";
		s.dataset.emoji = b.emoji;
		s.textContent = b.emoji + " " + b.count;
		parts.push(s);
	}
	if (more) {
		// Overflow marker: more non-zero configured emojis than the
		// badge cap — same contract as the server template's
		// {{if .MoreReactions}} span.
		const s = document.createElement("span");
		s.className = "react more";
		s.title = "more reactions";
		s.textContent = "…";
		parts.push(s);
	}
	strip.replaceChildren(...parts);
}

// onImageLiked updates a card's tallies when any visitor toggles a
// vote (SSE image-liked, published after the toggle commits — likes
// AND dislikes ride one event because every toggle can move both).
// The detached set is swept too, mirroring onThumbReady: restore()
// reattaches trimmed cards with zero network traffic, so one left
// behind would show stale counts until the next full reload.
function onImageLiked(ev) {
	if (!grid || !ev || !ev.id) return;
	if (!Number.isFinite(ev.likes) || !Number.isFinite(ev.dislikes)) return;
	const card = grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]');
	if (card) setCardCounts(card, ev.likes, ev.dislikes);
	for (let i = detached.length - 1; i >= 0; i--) {
		if (detached[i].dataset.id === ev.id) setCardCounts(detached[i], ev.likes, ev.dislikes);
	}
}

// onImageReacted rebuilds a card's reaction badges when any visitor
// toggles a reaction (SSE image-reacted, published after the commit
// with the FULL absolute tally map — dormant names arrive too and are
// filtered by topBadges' glyph lookup). Detached sweep like the
// other card updaters.
function onImageReacted(ev) {
	if (!grid || !ev || !ev.id || !ev.reactions) return;
	const card = grid.querySelector('article.card[data-id="' + CSS.escape(ev.id) + '"]');
	if (card) setCardReactions(card, ev.reactions);
	for (let i = detached.length - 1; i >= 0; i--) {
		if (detached[i].dataset.id === ev.id) setCardReactions(detached[i], ev.reactions);
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
		if (sortLiked) {
			// Top-rated mode: flushing would prepend into a score-
			// sorted grid and lie about the order. Reload re-sorts
			// server-side and clears the buffer with it. Deliberately
			// checked BEFORE filterClearFn: on a liked page whose user
			// typed a query (search mode over a still-sortLiked module),
			// this reloads the /search URL — preserving the user's
			// query context — rather than exiting the search; exiting
			// to "/?sort=liked" unilaterally would discard more context
			// than the click promised.
			location.reload();
			return;
		}
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
			"image-liked": onImageLiked,
			"image-reacted": onImageReacted,
			// Overflow / replay-gap recovery: full refetch is always correct.
			onReset: () => location.reload(),
		},
		{ since: document.body.dataset.lastEvent }
	);
}
