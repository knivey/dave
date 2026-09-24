// image.js: live reveal of the arrival-direction chevron plus keyboard
// navigation on the image details page.
//
// The server renders static prev/next chevrons for no-JS users; this
// module only ADDS what render time couldn't know: when the page was
// rendered as the newest image (body[data-at-end], i.e. no prev/newer
// neighbor existed yet), new arrivals extend its navigation, so on
// image-new it fetches /api/images/<id>/neighbors once and fades in
// the chevron pointing at the newcomer.
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
	// parse.
	document.addEventListener("keydown", (e) => {
		if (e.ctrlKey || e.metaKey || e.altKey) return;
		const t = e.target;
		if (t && /INPUT|TEXTAREA|SELECT/.test(t.tagName)) return;
		if (t && t.isContentEditable) return;
		if (e.key === "ArrowLeft" || e.key === "p") {
			const prev = document.getElementById("nav-prev");
			if (prev) window.location.href = prev.href;
		} else if (e.key === "ArrowRight" || e.key === "n") {
			const next = document.getElementById("nav-next");
			if (next) window.location.href = next.href;
		}
	});

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
