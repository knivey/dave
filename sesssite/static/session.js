// session.js — live updates for the session page (Task 10). Opens the
// SSE stream positioned at the page's newest row (?since), appends
// 'messages' events (rendered row HTML) to #msgs with stick-to-bottom
// scrolling, updates the status badge on 'session' events, and pages
// older rows in via ?fragment=rows with scroll-preserving prepend.
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
