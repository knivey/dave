// user.js — live updates for the user page (Task 10). Listens for
// 'updated' pings on the user's SSE stream and refetches the first
// page of the session list as a fragment; also pages older sessions in
// via the (before_last_active, before_id) keyset cursor.
(function () {
	const slug = document.body.dataset.userSlug;
	if (!slug) return;
	const list = document.getElementById("session-list");

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

	const es = new EventSource("/u/" + slug + "/events");
	es.addEventListener("updated", () => {
		fetch(location.pathname + "?fragment=list")
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html || !list) return;
				const { rows } = listFragment(html);
				if (rows) list.innerHTML = rows.innerHTML;
			});
	});

	const older = document.getElementById("user-load-older");
	if (older) older.addEventListener("click", () => {
		fetch(location.pathname + "?fragment=list&before_last_active=" +
			encodeURIComponent(older.dataset.lastActive) +
			"&before_id=" + older.dataset.id)
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html || !list) return;
				const { rows, older: next } = listFragment(html);
				if (!rows || !rows.children.length) { older.hidden = true; return; }
				// snapshot first: appendChild moves nodes out of the
				// fragment's live collection as it iterates
				for (const li of [...rows.children]) list.appendChild(li);
				if (next) {
					older.dataset.lastActive = next.dataset.lastActive;
					older.dataset.id = next.dataset.id;
				} else {
					older.hidden = true;
				}
			});
	});
})();
