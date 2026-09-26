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
		const zimg = overlay.querySelector("img");
		// Unlock the CSS state machine: from here on visibility is
		// class-owned (hidden attr gone), so the fade transition can
		// run in both directions without display juggling.
		overlay.removeAttribute("hidden");

		const isOpen = () => overlay.classList.contains("open");

		function setOpen(on) {
			overlay.classList.toggle("open", on);
			document.body.classList.toggle("zoom-open", on);
			zoomToggle.setAttribute("aria-expanded", on ? "true" : "false");
			if (!on) resetGestureZoom();
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

		// ---- Touch magnification (mobile audit #5) ----
		// The overlay's contain-fit renders the image at ~1× of its
		// fit state on phones, and touch has neither the zoom-out
		// cursor hint nor Esc — the overlay was a same-size picture
		// you could only dismiss. This layer adds the gestures a phone
		// user expects, ALL via pointer events on the overlay:
		//   double-tap → 2× zoom centered on the tap point
		//   drag (zoomed) → pan, clamped so the image never leaves
		//   pinch (two pointers) → continuous 1–8×, anchored under the
		//     fingers; lifting one finger continues as a pan
		//   single tap (zoomed) → reset to 1×
		//   single tap (not zoomed) → close, DELAYED one double-tap
		//     window (~330ms) so the second tap of a double-tap can
		//     cancel it
		// Mouse keeps its old contract (any left press closes; drag
		// while zoomed pans instead), and keyboard ✕ activation still
		// closes through the click listener's e.detail === 0 branch.
		// touch-action: none on the overlay/img (CSS) is what keeps
		// the browser from claiming the moves for page panning.
		//
		// Transform model: the img element (width/height 100% of the
		// overlay content box) carries
		// translate(x,y) scale(s) with transform-origin 0 0, so
		// element point e lands at x + e*s. Panning math therefore
		// works in element coordinates; clamping uses the letterboxed
		// PAINTED rect (contain fit of naturalWidth/Height inside the
		// content box) so the image, not the letterbox, is what stays
		// under the viewport.
		const z = { s: 1, x: 0, y: 0 };
		let gesture = null; // active touch/mouse gesture (see pointerdown)
		const pointers = new Map(); // pointerId -> last client position
		let pendingClose = null; // single-tap close timer (double-tap window)
		let suppressClickUntil = 0; // gesture-produced clicks (expiry-dated)
		let lastTap = { t: 0, x: 0, y: 0 };
		// A tap that reset the zoom consumes the NEXT tap, but only
		// inside the double-tap window (tapAfterResetAt + DBL_TAP_MS —
		// the same duration and performance.now() clock the double-tap
		// detector uses): the classic lightbox contract is that a
		// double-tap while zoomed zooms OUT (first tap resets, second
		// tap is its partner) — without the consumption rule, the
		// partner tap armed the single-tap close timer and a
		// pinch-then-double-tap dismissed the overlay entirely; without
		// the time bound, a tap-to-close made seconds after a zoom-out
		// hit the stale consumed flag and died as a no-op.
		let tapAfterResetAt = 0;
		const MAX_SCALE = 8;
		const TAP_SLOP = 8; // px of movement before a press stops being a tap
		const DBL_TAP_MS = 320;
		const DBL_TAP_RADIUS = 48;

		// baseBox measures the img's UNTRANSFORMED geometry (overlay
		// content box + the contain-fit painted rect inside it),
		// independent of the current transform — the overlay itself is
		// never transformed. x0/y0 are the content box's client-space
		// origin; pointer client coords minus them are element coords.
		function baseBox() {
			const o = overlay.getBoundingClientRect();
			const cs = getComputedStyle(overlay);
			const pl = parseFloat(cs.paddingLeft) || 0;
			const pt = parseFloat(cs.paddingTop) || 0;
			const w = o.width - pl - (parseFloat(cs.paddingRight) || 0);
			const h = o.height - pt - (parseFloat(cs.paddingBottom) || 0);
			const nw = zimg.naturalWidth || w;
			const nh = zimg.naturalHeight || h;
			const fit = Math.min(w / nw, h / nh);
			const dw = nw * fit;
			const dh = nh * fit;
			return { x0: o.left + pl, y0: o.top + pt, w, h, ox: (w - dw) / 2, oy: (h - dh) / 2, dw, dh };
		}

		function applyZoom() {
			zimg.style.transformOrigin = "0 0";
			zimg.style.transform = z.s === 1 ? "" : "translate(" + z.x + "px, " + z.y + "px) scale(" + z.s + ")";
			overlay.classList.toggle("zoomed", z.s > 1);
		}

		// clampT bounds the pan so the scaled PAINTED rect cannot leave
		// the viewport while it is larger than it (an axis where it is
		// smaller centers the element — the painting is centered in the
		// element, so it centers on screen).
		function clampT(b) {
			if (z.s <= 1) {
				z.s = 1;
				z.x = 0;
				z.y = 0;
				return;
			}
			if (b.dw * z.s <= b.w) z.x = (b.w - b.w * z.s) / 2;
			else z.x = Math.min(Math.max(z.x, b.w - z.s * (b.ox + b.dw)), -z.s * b.ox);
			if (b.dh * z.s <= b.h) z.y = (b.h - b.h * z.s) / 2;
			else z.y = Math.min(Math.max(z.y, b.h - z.s * (b.oy + b.dh)), -z.s * b.oy);
		}

		// zoomTo scales to s1 keeping the viewport point (cx, cy)
		// pinned: the element coordinate under it stays under it.
		function zoomTo(cx, cy, s1, b) {
			const ex = (cx - b.x0 - z.x) / z.s;
			const ey = (cy - b.y0 - z.y) / z.s;
			z.s = s1;
			z.x = cx - b.x0 - ex * s1;
			z.y = cy - b.y0 - ey * s1;
			clampT(b);
			applyZoom();
		}

		function resetGestureZoom() {
			z.s = 1;
			z.x = 0;
			z.y = 0;
			gesture = null;
			pointers.clear();
			tapAfterResetAt = 0;
			if (pendingClose) {
				clearTimeout(pendingClose);
				pendingClose = null;
			}
			lastTap = { t: 0, x: 0, y: 0 };
			applyZoom();
		}

		function dist(ax, ay, bx, by) {
			return Math.hypot(ax - bx, ay - by);
		}

		overlay.addEventListener("pointerdown", (e) => {
			if (!isOpen()) return;
			// Capture per pointer so moves/ups keep targeting the
			// overlay even outside it (multi-pointer safe: capture is
			// keyed by pointerId).
			try {
				overlay.setPointerCapture(e.pointerId);
			} catch {
				/* pointer already gone: the subsequent up is a no-op */
			}
			pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
			if (pointers.size === 1) {
				// A new press cancels any single-tap close still inside
				// the double-tap window — either this press is the
				// double-tap's second tap, or a fresh interaction that
				// should not race the timer.
				if (pendingClose) {
					clearTimeout(pendingClose);
					pendingClose = null;
				}
				gesture = { kind: "tap", startX: e.clientX, startY: e.clientY, base: baseBox() };
			} else if (pointers.size === 2) {
				// Second finger: a pinch, never a delayed close.
				if (pendingClose) {
					clearTimeout(pendingClose);
					pendingClose = null;
				}
				const [p1, p2] = [...pointers.values()];
				const b = (gesture && gesture.base) || baseBox();
				// Anchor: the element coordinate under the initial
				// midpoint stays under the moving midpoint.
				const anchor = {
					x: ((p1.x + p2.x) / 2 - b.x0 - z.x) / z.s,
					y: ((p1.y + p2.y) / 2 - b.y0 - z.y) / z.s,
				};
				gesture = { kind: "pinch", d0: dist(p1.x, p1.y, p2.x, p2.y) || 1, s0: z.s, anchor, base: b };
			}
		});

		overlay.addEventListener("pointermove", (e) => {
			if (!pointers.has(e.pointerId) || !gesture) return;
			pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
			if (gesture.kind === "tap") {
				if (dist(e.clientX, e.clientY, gesture.startX, gesture.startY) > TAP_SLOP) {
					// Past the slop it is a drag: panning (and cursor
					// feedback) only mean anything while zoomed; an
					// unzoomed drag is a dead gesture (no close on up).
					gesture.kind = z.s > 1 ? "pan" : "drag";
					gesture.panFrom = { x: e.clientX, y: e.clientY };
					if (gesture.kind === "pan") overlay.classList.add("dragging");
				}
				return;
			}
			if (gesture.kind === "pan") {
				z.x += e.clientX - gesture.panFrom.x;
				z.y += e.clientY - gesture.panFrom.y;
				gesture.panFrom = { x: e.clientX, y: e.clientY };
				clampT(gesture.base);
				applyZoom();
				return;
			}
			if (gesture.kind === "pinch" && pointers.size >= 2) {
				const [p1, p2] = [...pointers.values()];
				const d = dist(p1.x, p1.y, p2.x, p2.y) || 1;
				const mx = (p1.x + p2.x) / 2;
				const my = (p1.y + p2.y) / 2;
				z.s = Math.min(MAX_SCALE, Math.max(1, gesture.s0 * (d / gesture.d0)));
				z.x = mx - gesture.base.x0 - gesture.anchor.x * z.s;
				z.y = my - gesture.base.y0 - gesture.anchor.y * z.s;
				clampT(gesture.base);
				applyZoom();
			}
		});

		function onPointerEnd(e, fromUp) {
			if (!pointers.has(e.pointerId)) return;
			const kind = gesture && gesture.kind;
			pointers.delete(e.pointerId);
			overlay.classList.remove("dragging");
			if (pointers.size === 1 && kind === "pinch") {
				// One finger lifted mid-pinch: continue as a pan with
				// the survivor, from its current position.
				const [p] = [...pointers.values()];
				gesture = { kind: "pan", panFrom: { x: p.x, y: p.y }, base: gesture.base };
				return;
			}
			if (pointers.size > 0) return;
			gesture = null;
			if (kind === "pinch" || kind === "pan" || kind === "drag") {
				// Consumed by the gesture layer; the compatibility
				// click the engine may still fire must not close.
				// Expiry-dated (not one-shot) so a click the engine
				// never fires cannot swallow a later real one.
				suppressClickUntil = performance.now() + 500;
				if (z.s <= 1) resetGestureZoom();
				return;
			}
			// pointercancel is the engine reclaiming the pointer (a
			// system gesture) — never reward it with a close.
			if (kind === "tap" && fromUp) handleTap(e);
		}

		overlay.addEventListener("pointerup", (e) => onPointerEnd(e, true));
		overlay.addEventListener("pointercancel", (e) => onPointerEnd(e, false));

		// hitClose: pointer capture retargets pointer events to the
		// overlay, so e.target cannot identify the ✕ — hit-test the
		// release point instead.
		function hitClose(cx, cy) {
			const el = document.elementFromPoint(cx, cy);
			return !!(el && el.closest && el.closest(".zoom-close"));
		}

		function handleTap(e) {
			if (!isOpen()) return;
			// The ✕ always closes, zoomed or not — a tap that only
			// reset the zoom would hide the one visible exit.
			if (hitClose(e.clientX, e.clientY)) {
				setOpen(false);
				lastTap = { t: 0, x: 0, y: 0 };
				return;
			}
			if (e.pointerType !== "touch") {
				// Mouse/pen tap: immediate close, no double-tap game.
				setOpen(false);
				return;
			}
			const now = performance.now();
			if (z.s > 1) {
				// Tap while zoomed: back to 1× (double-tap and ✕ exit
				// fully; pan readers get an easy un-zoom too). The
				// timestamp makes a rapid second tap (the double-tap
				// partner) purely a zoom-out instead of arming the
				// close timer — and expires with the same window, so a
				// much later tap-to-close is not eaten by a stale flag.
				resetGestureZoom();
				tapAfterResetAt = now;
				return;
			}
			if (tapAfterResetAt && now - tapAfterResetAt < DBL_TAP_MS) {
				// Partner tap of a zoom-out double-tap (inside the
				// window): consumed.
				tapAfterResetAt = 0;
				lastTap = { t: 0, x: 0, y: 0 };
				return;
			}
			const isDouble =
				now - lastTap.t < DBL_TAP_MS && dist(e.clientX, e.clientY, lastTap.x, lastTap.y) < DBL_TAP_RADIUS;
			lastTap = { t: now, x: e.clientX, y: e.clientY };
			if (isDouble) {
				lastTap = { t: 0, x: 0, y: 0 };
				zoomTo(e.clientX, e.clientY, 2, baseBox());
				return;
			}
			// Single tap: close after the double-tap window so a quick
			// second tap can cancel it and zoom instead.
			if (pendingClose) clearTimeout(pendingClose);
			pendingClose = setTimeout(() => {
				pendingClose = null;
				if (isOpen()) setOpen(false);
			}, DBL_TAP_MS + 10);
		}

		zoomToggle.addEventListener("click", () => setOpen(!isOpen()));

		// Keyboard activation of the ✕ (Enter/Space while focused)
		// closes here: e.detail === 0 marks non-pointer clicks. Mouse
		// and touch activations are fully owned by the pointer layer
		// above (touch needs the delayed-close window; mouse closes on
		// pointerup), so their compatibility click events are ignored.
		// The gesture suppression above must NEVER swallow the
		// detail === 0 branch: keyboard clicks are never
		// gesture-produced (no pan/pinch/drag dispatches one), and this
		// listener is the keyboard's ONLY close path — a Enter press
		// landing within 500ms after a pan would otherwise die as a
		// no-op.
		overlay.addEventListener("click", (e) => {
			if (e.detail !== 0 && performance.now() < suppressClickUntil) return;
			if (e.detail === 0 && isOpen()) setOpen(false);
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
