// session.js — live updates for the session page (Task 10). Opens the
// SSE stream positioned at the page's newest row (?since), appends
// 'messages' events (rendered row HTML) to #msgs with stick-to-bottom
// scrolling, updates the status badge on 'session' events, and pages
// older rows in via ?fragment=rows with scroll-preserving prepend.
// Also wires code-copy affordances: a hover-revealed button on every
// <pre> (ported from the paste site) and click-to-copy on inline <code>
// — a MutationObserver keeps both working on rows that arrive after
// load (SSE append, load-older prepend).
//
// RECONNECT CONTRACT: EventSource auto-reconnects with the URL it was
// CONSTRUCTED with — a since frozen at page load. After any mid-view
// stream drop (proxy timeout, sleep/wake, server restart) the server's
// gap-fill would then re-send every row since load, and appending
// those again would duplicate the transcript. onerror therefore checks
// readyState: CONNECTING means the browser is about to auto-reconnect
// with that stale URL — close() the stream and construct a fresh one
// whose since is the CURRENT newest row id, so gap-fill sends only the
// missing rows. The idempotency filter in the messages handler is the
// second net (any element whose id is already in the document is
// skipped — defends against every replay source). CLOSED (fatal — e.g.
// the per-IP cap's 503) is deliberately left alone: the browser has
// given up, and reconnecting manually would bypass its retry pacing.
(function () {
	const script = document.currentScript;
	const slug = script && script.dataset.slug;
	const list = document.getElementById("msgs");
	if (!slug || !list) return;

	const msgRows = () => list.querySelectorAll(".msg[id]");
	const lastId = () => {
		const rows = msgRows();
		return rows.length ? parseInt(rows[rows.length - 1].id.slice(1), 10) : 0;
	};
	const firstId = () => {
		const rows = msgRows();
		return rows.length ? parseInt(rows[0].id.slice(1), 10) : 0;
	};
	const pinned = () =>
		window.innerHeight + window.scrollY >= document.body.scrollHeight - 40;

	// ---- code copy (paste-site parity) --------------------------------
	function clipboardText(text, onDone) {
		if (!navigator.clipboard) return; // insecure context: no-op
		navigator.clipboard.writeText(text).then(onDone, function () {});
	}

	function wireCopyButtons(root) {
		// root may be a <pre> itself or a subtree containing them.
		const pres = root.matches && root.matches("pre")
			? [root]
			: root.querySelectorAll ? root.querySelectorAll("pre") : [];
		for (const pre of pres) {
			if (pre.querySelector(":scope > .copy-btn")) continue;
			// Stash the payload NOW, before the button's own text
			// becomes part of pre.textContent.
			const code = pre.querySelector("code");
			const text = (code || pre).textContent;
			const btn = document.createElement("button");
			btn.className = "copy-btn";
			btn.type = "button";
			btn.textContent = "Copy";
			btn.dataset.copyText = text;
			pre.appendChild(btn);
		}
	}

	// One delegated listener covers both affordances for every row, no
	// matter when it arrived: buttons exist only because the observer
	// created them, but their CLICKS bubble through #msgs.
	list.addEventListener("click", (ev) => {
		const btn = ev.target.closest(".copy-btn");
		if (btn) {
			clipboardText(btn.dataset.copyText || "", () => {
				btn.textContent = "Copied!";
				setTimeout(() => { btn.textContent = "Copy"; }, 1500);
			});
			return;
		}
		const code = ev.target.closest("code");
		if (code && !code.closest("pre")) {
			clipboardText(code.textContent, () => {
				code.classList.add("copied");
				setTimeout(() => code.classList.remove("copied"), 700);
			});
		}
	});

	// Rows land after load via SSE append and load-older prepend — the
	// observer wires their <pre>s the moment they enter #msgs.
	wireCopyButtons(list);
	new MutationObserver((muts) => {
		for (const m of muts) {
			for (const n of m.addedNodes) {
				if (n.nodeType === Node.ELEMENT_NODE) wireCopyButtons(n);
			}
		}
	}).observe(list, { childList: true, subtree: true });

	function connect() {
		const es = new EventSource("/s/" + slug + "/events?since=" + lastId());
		// ev.data is the payload itself — EventSource already unwrapped
		// the "data: " framing (and re-joined multi-line payloads), so
		// never re-parse a prefix off it.
		es.addEventListener("messages", (ev) => {
			const stick = pinned();
			const tpl = document.createElement("template");
			tpl.innerHTML = ev.data;
			let added = false;
			for (const el of [...tpl.content.children]) {
				// replayed rows carry an id already in the document
				if (el.id && document.getElementById(el.id)) continue;
				// toolpair replay filter: pairing consumes result rows
				// server-side, so the DOM's newest id can sit BELOW ids
				// the server already sent — a reconnect gap-fill then
				// re-emits the consumed result as an ORPHAN row. Skip it
				// when its PAIRED toolpair already rendered (a bare
				// .toolcall with the id means the result is still
				// pending — that orphan MUST land).
				const orphan = el.querySelector && el.querySelector(".toolresult[data-callid]");
				if (orphan) {
					const cid = orphan.dataset.callid;
					if (cid && list.querySelector(".toolpair[data-callid=\"" + CSS.escape(cid) + "\"]")) continue;
				}
				list.appendChild(el);
				added = true;
			}
			if (added && stick) window.scrollTo(0, document.body.scrollHeight);
		});
		es.addEventListener("session", (ev) => {
			try {
				const st = JSON.parse(ev.data);
				const el = document.getElementById("status-badge");
				if (el && st.status) {
					el.textContent = st.status;
					// keep the CSS color (badge status-<status>) in sync
					el.className = "badge status-" + st.status;
				}
			} catch (e) { /* ignore malformed payloads */ }
		});
		es.onerror = () => {
			if (es.readyState === EventSource.CONNECTING) {
				// the browser is about to auto-reconnect with the stale
				// page-load URL — take over with a fresh since
				es.close();
				connect();
			}
		};
	}
	connect();

	const older = document.getElementById("load-older");
	if (older) older.addEventListener("click", () => {
		fetch("/s/" + slug + "?fragment=rows&before=" + firstId())
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html) { older.hidden = true; return; }
				const stick = pinned();
				const prevHeight = document.body.scrollHeight;
				list.insertAdjacentHTML("afterbegin", html);
				// prepending grows the document upward; hold the viewport
				// on the same content unless the reader was at the bottom
				if (!stick) window.scrollBy(0, document.body.scrollHeight - prevHeight);
			});
	});
})();
