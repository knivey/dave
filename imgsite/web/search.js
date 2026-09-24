// search.js (milestone 5): as-you-type search over the gallery grid.
//
// Design: the server renders complete results pages at /search?q=… so
// no-JS and shared links work. With JS, typing swaps the grid contents
// with /search-fragment pages (first page on query change, then
// gallery.js's infinite scroll pages through the search fragment URL
// provider this module installs), updates the address bar via
// history.replaceState, and returns to the live gallery when the query
// empties. While a query is active, gallery.setFilterActive(true)
// buffers SSE arrivals behind the "+N new" pill (M4) — nothing is ever
// prepended into filtered results.
//
// Keystrokes are debounced (DEBOUNCE_MS) so one burst of typing fires
// one request, and superseded requests are aborted (AbortController) —
// a slow response landing after a newer query can never render: the
// modeSeq check in swapGrid drops it even if it somehow resolves.
"use strict";

import {
	setFilterActive,
	registerFilterClear,
	setFragmentURL,
	resetPaging,
	rewatchSentinel,
} from "./gallery.js";

// 300ms: typical inter-key gaps at normal typing speed run 100–300ms,
// so a 150ms debounce let most keystrokes clear the timer individually
// — one request + DB query per keystroke. 300ms coalesces mid-word
// bursts into one request per pause while still feeling instant.
const DEBOUNCE_MS = 300;

// isAbortError distinguishes "superseded by a newer query" from real
// fetch failures. Browsers reject an aborted fetch with a DOMException
// whose name is "AbortError"; it carries no information worth warning
// about and must not mark the current query as failed.
function isAbortError(err) {
	return !!err && err.name === "AbortError";
}

export function boot() {
	const form = document.getElementById("search-form");
	const box = document.getElementById("search-box");
	const grid = document.getElementById("grid");
	if (!form || !box || !grid) return;

	// data-gallery-title carries the site title on BOTH entry pages
	// (the gallery template binds it to .Title — the site title — and
	// the search template to .SiteTitle), with document.title as the
	// no-attribute fallback. Two uses: restoring the gallery <title>
	// on filter exit, and suffixing the client-side search title so it
	// matches the server's searchPageTitle format exactly
	// ("search: q — site title").
	const siteTitle = document.body.dataset.galleryTitle || document.title;
	let currentQuery = box.value.trim();
	// Last attempted swap (query or gallery restore) failed? Enter on
	// the same query must still resubmit: applyQuery's identical-query
	// guard exists to debounce, not to trap a failed result on screen.
	let lastFetchFailed = false;
	let timer = null;
	// Monotonic mode counter: fetches resolve out of order under rapid
	// typing (and a gallery restore can race a fresh query), so every
	// async grid swap validates it is still the LATEST requested mode
	// before touching the DOM/URL/title.
	let modeSeq = 0;
	// AbortController for the in-flight swap. The mode check above
	// already makes a late stale response harmless; aborting ALSO stops
	// the superseded fetch itself — the server result stops mattering
	// and the client stops downloading a response nobody will render.
	let inflight = null;

	function searchFragmentURL(q, cursor) {
		let u = "/search-fragment?q=" + encodeURIComponent(q);
		if (cursor) u += "&after=" + encodeURIComponent(cursor);
		return u;
	}

	// swapGrid replaces the grid contents with a fetched fragment's
	// body (cards + sentinel + optional no-results note), drops stale
	// paging/trim state, and re-arms the scroll observer. mySeq is the
	// modeSeq captured when the operation started; a mismatch means a
	// newer query (or restore) superseded it and the swap is dropped.
	// Starting a new swap aborts the previous in-flight fetch (it has
	// been superseded — its response can never render).
	async function swapGrid(url, mySeq) {
		if (inflight) inflight.abort();
		const ctl = new AbortController();
		inflight = ctl;
		try {
			const resp = await fetch(url, { signal: ctl.signal });
			if (!resp.ok) throw new Error("HTTP " + resp.status);
			const doc = new DOMParser().parseFromString(await resp.text(), "text/html");
			if (mySeq !== modeSeq) return false;
			resetPaging();
			grid.replaceChildren(...doc.body.childNodes);
			rewatchSentinel();
			return true;
		} finally {
			if (inflight === ctl) inflight = null;
		}
	}

	async function runSearch(q) {
		const mySeq = modeSeq;
		try {
			if (!(await swapGrid(searchFragmentURL(q), mySeq))) return;
			lastFetchFailed = false;
			history.replaceState(null, "", "/search?q=" + encodeURIComponent(q));
			document.title = "search: " + q + " — " + siteTitle;
		} catch (err) {
			// A superseded fetch rejects with AbortError: the newer
			// query owns the grid and its own outcome — this is not a
			// failure and must not arm the identical-query retry hatch.
			if (isAbortError(err)) return;
			lastFetchFailed = true;
			console.warn("search fetch failed", err);
		}
	}

	async function restoreGallery() {
		const mySeq = modeSeq;
		try {
			if (!(await swapGrid("/gallery", mySeq))) return;
		} catch (err) {
			if (isAbortError(err)) return; // superseded, not failed
			lastFetchFailed = true;
			console.warn("gallery restore failed", err);
			return; // keep the filter active: results grid is still shown
		}
		lastFetchFailed = false;
		setFragmentURL(null);
		// Clear the filter only after the live grid is back, so the
		// pill's buffered arrivals flush into the restored gallery.
		setFilterActive(false);
		history.replaceState(null, "", "/");
		document.title = siteTitle;
	}

	function enterResultsMode(q) {
		// Infinite scroll pages through the query-scoped fragment; SSE
		// arrivals buffer behind the pill instead of prepending.
		setFragmentURL((cursor) => searchFragmentURL(q, cursor));
		setFilterActive(true);
	}

	function applyQuery(q) {
		// Identical-query guard (debounce), with an escape hatch: a
		// FAILED fetch never updated the grid, so resubmitting the same
		// query is the user's only retry path (Enter after a failure).
		if (q === currentQuery && !lastFetchFailed) return;
		currentQuery = q;
		modeSeq++; // supersede any in-flight swap
		if (!q) {
			restoreGallery();
			return;
		}
		enterResultsMode(q);
		runSearch(q);
	}

	// The pill's click path (gallery.js) defers entirely to this
	// registered clear action: reset the box and fall through to the
	// gallery restore. The buffered arrivals flush only when that
	// restore succeeds (its setFilterActive(false) call) and stay
	// buffered behind the still-active filter if it fails.
	registerFilterClear(() => {
		box.value = "";
		applyQuery("");
	});

	// A server-rendered /search?q= page boots straight into results
	// mode so the sentinel pages through the search fragment and SSE
	// arrivals buffer from the first moment.
	if (currentQuery) enterResultsMode(currentQuery);

	box.addEventListener("input", () => {
		clearTimeout(timer);
		timer = setTimeout(() => applyQuery(box.value.trim()), DEBOUNCE_MS);
	});

	// Enter (or mobile "go") submits; with JS on, run immediately —
	// without JS the form GETs /search and the server renders the page.
	form.addEventListener("submit", (e) => {
		e.preventDefault();
		clearTimeout(timer);
		applyQuery(box.value.trim());
	});
}
