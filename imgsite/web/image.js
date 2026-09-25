// image.js: click-to-zoom on the main image, live reveal of the
// arrival-direction chevron, and keyboard navigation on the image
// details page.
//
// The server renders a complete page for no-JS users (fit-state main
// image, static chevrons, "open file" anchor); this module only ADDS
// what render time couldn't know: the zoom toggle (class flip + inline
// scale-up sizing — see the zoom block below) and, when the page was
// rendered as the newest image (body[data-at-end], i.e. no prev/newer
// neighbor existed yet), the live chevron — on image-new it fetches
// /api/images/<id>/neighbors once and fades in the chevron pointing at
// the newcomer.
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

	// ---- Click-to-zoom on the main image (owner request, Sep 2026) ----
	// The page serves the original bytes as the main <img>; the display
	// thumb remains an og:image derivative. The toggle is a real
	// <button>, so Enter/Space activation comes free and the no-JS page
	// renders the same fit-state image, just non-interactive — the raw
	// file stays one click away via the "open file" anchor in the topnav.
	//
	// Zoomed semantics, per the owner:
	//   - image larger than the zoom box (either dimension): show it at
	//     NATURAL size and pan by scrolling the page. The .zoomed class
	//     lifts the fit-state max-width/max-height caps; horizontal
	//     alignment is owned entirely by CSS (`justify-content: safe
	//     center` on .viewer start-aligns the item only when it
	//     overflows, so the overflowing side stays reachable by page
	//     scroll while tall-but-narrow portraits stay centered — no
	//     JS-managed pan class to keep in sync).
	//   - image smaller than the page: scale UP to fit (contain). Done
	//     with inline width/height here because CSS max-* caps can only
	//     shrink, never enlarge — one computed pair per toggle, no
	//     resize listener (re-toggling after a rotate/resize recomputes).
	const zoomToggle = document.getElementById("zoom-toggle");
	if (zoomToggle) {
		const zoomImg = zoomToggle.querySelector("img");
		const isZoomed = () => viewer.classList.contains("zoomed");

		// The zoom box is the viewer's actual on-screen box, NOT
		// window.inner*: the topnav sits above the viewer and the
		// scrollbar gutter eats into innerWidth, so window metrics
		// would "fit" images that then induce a horizontal scrollbar or
		// hide partly under the fold. Horizontal: clientWidth (content
		// width — the scrollbar is already excluded by layout).
		// Vertical: distance from the viewer's top edge to the fold,
		// with the top clamped at 0 (a zoom triggered while scrolled
		// past the viewer via keyboard must not enlarge the budget) and
		// a 1px floor so degenerate measures keep the scale math finite.
		const zoomBoxWidth = () => viewer.clientWidth;
		const zoomBoxHeight = () =>
			Math.max(window.innerHeight - Math.max(viewer.getBoundingClientRect().top, 0), 1);

		// Classification reads naturalWidth/naturalHeight, but the
		// RENDERED "natural size" in the zoomed state is the
		// width/height ATTRIBUTE size whenever the template shipped
		// dims (attributes are presentational hints). The two can
		// diverge — e.g. a post-latent upscale workflow whose saved
		// file differs from the graph dims stored in the row. Worst
		// case is a mispicked branch or an inline size that doesn't
		// exactly match the attrs; safe-center alignment corrects the
		// overflow side on its own, so panning never breaks.
		// naturalWidth is 0 until the image decodes: that reads as
		// "not larger", and the 0-guard in applyZoomSize skips the
		// scale-up styling — a pre-load toggle only flips the class,
		// and the load listener below re-runs the classification.
		const panZoom = () =>
			!!zoomImg &&
			(zoomImg.naturalWidth > zoomBoxWidth() ||
				zoomImg.naturalHeight > zoomBoxHeight());

		function applyZoomSize(on) {
			if (!zoomImg) return;
			if (on && !panZoom() && zoomImg.naturalWidth > 0) {
				const scale = Math.min(
					zoomBoxWidth() / zoomImg.naturalWidth,
					zoomBoxHeight() / zoomImg.naturalHeight
				);
				zoomImg.style.width = Math.round(zoomImg.naturalWidth * scale) + "px";
				zoomImg.style.height = Math.round(zoomImg.naturalHeight * scale) + "px";
				return;
			}
			// Fit state and natural-size (pan) state alike: no inline
			// sizing — the CSS caps (or their absence in .zoomed) own
			// the size, falling back to the width/height attrs /
			// intrinsic size.
			zoomImg.style.width = "";
			zoomImg.style.height = "";
		}

		function setZoom(on) {
			viewer.classList.toggle("zoomed", on);
			zoomToggle.setAttribute("aria-pressed", on ? "true" : "false");
			zoomToggle.title = on ? "zoom out (click or Esc)" : "zoom in";
			applyZoomSize(on);
		}

		zoomToggle.addEventListener("click", () => setZoom(!isZoomed()));

		// Re-classify once the bytes decode: a click before load cannot
		// see the natural size, so the branch decision (and any scale-up
		// sizing) would be stale. setZoom(true) re-reads everything and
		// is idempotent. load does not bubble — the listener lives on
		// the img itself.
		if (zoomImg) {
			zoomImg.addEventListener("load", () => {
				if (isZoomed()) setZoom(true);
			});
		}

		// Esc exits zoom. Deliberately NOT routed through the nav
		// listener's input/modifier guards: Esc is an explicit dismissal,
		// never a typing key, and exiting zoom while focus happens to
		// sit in the search box costs nothing.
		document.addEventListener("keydown", (e) => {
			if (e.key === "Escape" && isZoomed()) setZoom(false);
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
