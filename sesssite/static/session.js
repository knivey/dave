// session.js — live updates for the session page (Task 10). Opens the
// SSE stream positioned at the page's newest row (?since), appends
// 'messages' events (rendered row HTML) to #msgs with stick-to-bottom
// scrolling, updates the status badge on 'session' events, and pages
// older rows in via ?fragment=rows with scroll-preserving prepend.
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

	// ev.data is the payload itself — EventSource already unwrapped the
	// "data: " framing (and re-joined multi-line payloads), so never
	// re-parse a prefix off it.
	const es = new EventSource("/s/" + slug + "/events?since=" + lastId());
	es.addEventListener("messages", (ev) => {
		const stick = pinned();
		list.insertAdjacentHTML("beforeend", ev.data);
		if (stick) window.scrollTo(0, document.body.scrollHeight);
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
