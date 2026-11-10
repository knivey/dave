// user.js — live updates for the user page (Task 10). Listens for
// 'updated' pings on the user's SSE stream and refetches the first
// page of the session list as a fragment; also pages older sessions in
// via the (before_last_active, before_id) keyset cursor.
(function () {
	const slug = document.body.dataset.userSlug;
	if (!slug) return;
	const list = document.getElementById("session-list");
	const older = document.getElementById("user-load-older");

	// listFragment parses a ?fragment=list response. The fragment ships
	// a whole <ul id="session-list"> (plus the refreshed load-older
	// button when more pages exist); rows and button are adopted
	// piecewise — never the wrapper, which would nest a second
	// #session-list inside this one.
	const listFragment = (html) => {
		const tpl = document.createElement("template");
		tpl.innerHTML = html;
		return {
			rows: tpl.content.querySelector("#session-list"),
			older: tpl.content.querySelector("#user-load-older"),
		};
	};

	// CURSOR-ADOPTION CONTRACT: refetch must adopt the fragment's
	// load-older button state, not just its rows. Page 1's LAST row is
	// the new load-older frontier; keeping the pre-refetch dataset
	// would make load-older fetch from a stale (last_active, id) cursor
	// — every row between page 1's tail and that cursor becomes
	// unreachable, and the append path's dedupe below is insurance
	// against duplicates, not a fix for a wrong cursor.
	const adoptOlder = (fresh) => {
		if (!older) return;
		if (fresh) {
			older.dataset.lastActive = fresh.dataset.lastActive;
			older.dataset.id = fresh.dataset.id;
			older.hidden = false;
		} else {
			older.hidden = true;
		}
	};

	const es = new EventSource("/u/" + slug + "/events");
	// Reconnect catch-up: this stream carries change PINGS only — no
	// replay, no since. A change that happens while the stream is down
	// (including the render→connect window at page load) would be
	// swallowed, because the (re)connecting poller snapshots the
	// already-changed state as its baseline. Refetching page 1 on every
	// open closes the gap; the full replace is idempotent — the same
	// data renders the same DOM, so repeated pings and refetches can
	// never duplicate rows (unlike the session page's appends).
	const refetch = () => {
		fetch(location.pathname + "?fragment=list")
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html || !list) return;
				const { rows, older: fresh } = listFragment(html);
				if (rows) list.innerHTML = rows.innerHTML;
				adoptOlder(fresh);
			});
	};
	es.onopen = refetch;
	es.addEventListener("updated", refetch);

	if (older) older.addEventListener("click", () => {
		fetch(location.pathname + "?fragment=list&before_last_active=" +
			encodeURIComponent(older.dataset.lastActive) +
			"&before_id=" + older.dataset.id)
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html || !list) return;
				const { rows, older: next } = listFragment(html);
				if (!rows || !rows.children.length) { older.hidden = true; return; }
				// id-dedupe second net (session.js's replay filter):
				// a refetch that landed while paged into history re-arms
				// the cursor at page 1's tail, so a subsequent load-older
				// fetches rows already appended below — skip those. Rows
				// carry no id attribute; the session href keys them.
				const seen = new Set();
				for (const a of list.querySelectorAll("li.session-row > a")) {
					seen.add(a.getAttribute("href"));
				}
				// snapshot first: appendChild moves nodes out of the
				// fragment's live collection as it iterates
				for (const li of [...rows.children]) {
					const a = li.querySelector("a");
					if (a && seen.has(a.getAttribute("href"))) continue;
					list.appendChild(li);
				}
				if (next) {
					older.dataset.lastActive = next.dataset.lastActive;
					older.dataset.id = next.dataset.id;
				} else {
					older.hidden = true;
				}
			});
	});
})();
