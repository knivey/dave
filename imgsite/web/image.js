// image.js: fullscreen click-to-zoom overlay on the main image, live
// reveal of the arrival-direction chevron, and keyboard navigation on
// the image details page.
//
// The server renders a complete page for no-JS users (fit-state main
// image in its aspect box, static chevrons OUTSIDE the image, "open
// file" anchor, and the zoom overlay markup hidden); this module only
// ADDS what render time couldn't know: opening/closing the overlay
// (class flips only — the overlay's contain sizing is pure CSS, no
// dimension math) and, when the page was rendered as the newest image
// (body[data-at-end], i.e. no prev/newer neighbor existed yet), the
// live chevron — on image-new it fetches /api/images/<id>/neighbors
// once and fades in the chevron pointing at the newcomer.
//
// Direction note: new arrivals are always NEWER than the current
// image, and newer is the prev (left) chevron in this site's keyset
// naming (prev=newer, next=older — see the neighbors API). The plan's
// prose calls the revealed affordance "the next button" because the
// newcomer is the next thing to look at in arrival order; the element
// it maps to here is #nav-prev.
"use strict";

import { connect } from "./sse.js";

const NEIGHBORS_DEBOUNCE_MS = 300;

export function boot() {
	const viewer = document.querySelector(".viewer");
	const currentID = document.body.dataset.imageId;
	if (!viewer || !currentID) return;

	// Keyboard nav (←/→/n/p, modifier- and input-guarded), migrated
	// from the page's M2 inline script: the elements are looked up at
	// KEYSTROKE time, so a live-revealed chevron's href is picked up —
	// inline closed-over vars could never see an element created after
	// parse. While the zoom overlay is open, arrow/p/n nav is
	// suppressed (the overlay covers the page; Esc is its dismissal
	// key) so a stray ← behind the backdrop doesn't navigate away.
	document.addEventListener("keydown", (e) => {
		if (e.ctrlKey || e.metaKey || e.altKey) return;
		const t = e.target;
		if (t && /INPUT|TEXTAREA|SELECT/.test(t.tagName)) return;
		if (t && t.isContentEditable) return;
		const overlay = document.getElementById("zoom-overlay");
		if (overlay && overlay.classList.contains("open")) return;
		if (e.key === "ArrowLeft" || e.key === "p") {
			const prev = document.getElementById("nav-prev");
			if (prev) window.location.href = prev.href;
		} else if (e.key === "ArrowRight" || e.key === "n") {
			const next = document.getElementById("nav-next");
			if (next) window.location.href = next.href;
		}
	});

	// ---- Fullscreen zoom overlay (owner redesign, Sep 2026) ----
	// The overlay element is server-rendered and ships with the hidden
	// attribute (display: none for no-JS UAs — the CSS block's first
	// rule re-asserts [hidden] because the author display: flex would
	// otherwise override the UA rule). JS's entire job is class and
	// attribute flips: .open on the overlay fades it in (opacity does
	// the animating; visibility flips with 0s duration on open and 0s
	// delayed-to-fade-end on close — see the CSS comment for why that
	// asymmetry is load-bearing for the focus below), zoom-open
	// on <body> locks page scroll behind the fixed backdrop, and
	// aria-expanded on the toggle mirrors the state. Sizing inside the	// overlay is pure CSS (width/height 100% + object-fit: contain —
	// large images shrink, small ones scale up, both letterbox); there
	// is deliberately NO measurement math: the 2fedc24 zoom computed
	// scale against the viewer's on-screen box, so small images only
	// ever grew to box size, never screen size, and the whole
	// natural-size classification apparatus (pan branch, inline
	// sizing, load-time re-classification) existed only to feed it.
	const zoomToggle = document.getElementById("zoom-toggle");
	const overlay = document.getElementById("zoom-overlay");
	if (zoomToggle && overlay) {
		const closeBtn = document.getElementById("zoom-close");
		// Unlock the CSS state machine: from here on visibility is
		// class-owned (hidden attr gone), so the fade transition can
		// run in both directions without display juggling.
		overlay.removeAttribute("hidden");

		const isOpen = () => overlay.classList.contains("open");

		function setOpen(on) {
			overlay.classList.toggle("open", on);
			document.body.classList.toggle("zoom-open", on);
			zoomToggle.setAttribute("aria-expanded", on ? "true" : "false");
			// Focus: into the dialog on open (the close button — the
			// one tabbable control inside), back to the toggle on
			// close. The open-path focus waits one rAF because the
			// class flip and a synchronous focus() share a task —
			// focusability is checked against computed style, which
			// has not yet picked up the .open class at that instant.
			// This is only sound because the CSS transitions
			// visibility with 0s duration on OPEN (instant flip at the
			// first recalc): an animated visibility keeps computing
			// hidden at transition progress 0, and browser review
			// showed even a rAF callback can land inside that window,
			// silently dropping the focus under default motion
			// settings. On close the toggle is focused synchronously
			// (it never transitions — always visible), so the return
			// focus lands exactly as designed, and the isOpen() guard
			// keeps a fast open→close from stealing focus back to a
			// mid-fade ✕. No full focus trap: Esc, any click, and the
			// close button all dismiss, which covers keyboard exit.
			if (on) {
				if (closeBtn) {
					requestAnimationFrame(() => {
						if (isOpen()) closeBtn.focus();
					});
				}
			} else {
				zoomToggle.focus();
			}
		}

		zoomToggle.addEventListener("click", () => setOpen(!isOpen()));

		// Any click inside the overlay closes it: the backdrop, the
		// image itself, and the close button (the visible affordance —
		// the whole surface also shows the zoom-out cursor).
		overlay.addEventListener("click", () => {
			if (isOpen()) setOpen(false);
		});

		// Esc exits zoom. Deliberately NOT routed through the nav
		// listener's input/modifier guards: Esc is an explicit
		// dismissal, never a typing key, and exiting zoom while focus
		// happens to sit in the search box costs nothing.
		document.addEventListener("keydown", (e) => {
			if (e.key === "Escape" && isOpen()) setOpen(false);
		});
	}

	// Live reveal only applies at the newest end of the gallery.
	if (document.body.dataset.atEnd !== "true") return;

	let fetchTimer = null;
	let revealed = false;

	// First-connect replay cursor: body[data-last-event] (present
	// whenever a hub exists) is the event id this page's neighbors were
	// rendered at. An image-new in the render→subscribe gap would
	// otherwise never fire the reveal — connect() replays it via
	// ?since=<id>, and the debounced neighbors fetch reveals the
	// chevron exactly as a live arrival would. Absent attribute →
	// undefined → live-only first connect.
	connect(
		{
			"image-new": () => {
				if (revealed) return;
				// Bursts (multi-image jobs) debounce into one fetch.
				if (fetchTimer) clearTimeout(fetchTimer);
				fetchTimer = setTimeout(fetchNeighbors, NEIGHBORS_DEBOUNCE_MS);
			},
			// Overflow / replay-gap recovery: full refetch is always correct.
			onReset: () => location.reload(),
		},
		{ since: document.body.dataset.lastEvent }
	);

	async function fetchNeighbors() {
		fetchTimer = null;
		if (revealed) return;
		try {
			const resp = await fetch(
				"/api/images/" + encodeURIComponent(currentID) + "/neighbors"
			);
			if (!resp.ok) return;
			const data = await resp.json();
			if (data && data.prev && data.prev.id) revealPrev(data.prev.id);
		} catch {
			// Transient fetch failure: the next image-new event retries
			// via the debounce above.
		}
	}

	function revealPrev(id) {
		revealed = true;
		if (document.getElementById("nav-prev")) return; // never duplicate
		const a = document.createElement("a");
		a.className = "chevron left";
		a.id = "nav-prev";
		a.href = "/" + id;
		a.title = "newer (← / p)";
		a.textContent = "\u2039";
		// Reduced motion: skip the fade entirely and show the chevron
		// immediately (the transition itself is the motion).
		if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
			viewer.insertBefore(a, viewer.firstChild); // template order: prev chevron first
			return;
		}
		// Fade-in: start fully transparent, transition to opaque on the
		// next frame so the freshly-added element animates.
		a.style.opacity = "0";
		a.style.transition = "opacity 0.4s ease";
		viewer.insertBefore(a, viewer.firstChild); // template order: prev chevron first
		requestAnimationFrame(() => {
			a.style.opacity = "1";
		});
	}
}
