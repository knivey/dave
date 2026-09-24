// sse.js: /events EventSource wrapper.
//
// What native EventSource already covers: automatic reconnect on
// transient failure, and the Last-Event-ID header — the browser sends
// it by itself on those retries, which is exactly what the server's
// replay ring answers.
//
// What this wrapper adds:
//   - last-seen-id tracking. A FRESH EventSource object starts with no
//     id and no way to set headers, so when WE reopen the stream
//     (visibilitychange, bfcache restore, fatal-error backoff) the id
//     rides the URL as ?since=N instead — the server's other replay
//     entry point.
//   - visibilitychange: close when the tab hides, reopen when it
//     becomes visible. Frozen background tabs keep timers and sockets
//     alive only unpredictably; a deterministic close/reopen plus the
//     replay bridge is the documented approach.
//   - bfcache handling (pageshow/pagehide): clicking into an image
//     page and pressing back does NOT reload this page — the browser
//     freezes it into the back/forward cache and tears the
//     EventSource's socket down while frozen, with no error event and
//     usually no visibilitychange on restore (the tab may have stayed
//     visible the whole time). Without the pageshow hook the stream
//     would stay dead silently and every later image-new would be
//     missed — the classic "gallery stopped updating" bug.
//   - the "reset" event hook: the server sends it when this client's
//     buffer overflowed or its replay gap exceeded the ring —
//     full-state-refetch semantics, so the default handler reloads.
export function connect(handlers) {
	const names = Object.keys(handlers).filter((n) => n !== "onReset" && n !== "hello");
	let es = null;
	let lastId = 0;
	let closed = false;
	let reopenTimer = null;
	// Has open() run at least once? The FIRST connect of a page must
	// NOT ask for replay (the server just rendered the page's state;
	// replaying the whole ring — or triggering a reset-reload loop on a
	// busy hub — would be wrong). Every LATER open (visibility, bfcache
	// restore, 429 backoff) is resuming a STALE snapshot and must bridge
	// via ?since= — including since=0 when no event was ever received:
	// the server then replays the entire ring (or resets, correctly,
	// when even that can't reconstruct the page's gap).
	let openedOnce = false;

	function trackId(e) {
		const id = Number(e.lastEventId);
		if (Number.isFinite(id) && id > lastId) lastId = id;
	}

	function open() {
		// open() can be reached from the reopen timer AND the
		// visibilitychange path concurrently. Tear down whatever is
		// still armed or attached first: overwriting es without
		// closing it would leak a per-IP SSE slot on the server and
		// deliver duplicate events, and a stale armed timer could fire
		// yet another open() later. (Closing an already-CLOSED
		// EventSource is a no-op, so this is safe on every path.)
		if (reopenTimer) {
			clearTimeout(reopenTimer);
			reopenTimer = null;
		}
		if (es) es.close();
		const first = !openedOnce;
		openedOnce = true;
		// Reopen always carries since= (0 included: replay the whole
		// ring from the beginning of the page's stale snapshot).
		const url = first ? "/events" : "/events?since=" + lastId;
		const src = new EventSource(url);
		es = src;
		// hello is wrapper-internal bookkeeping (the server's
		// connect-time "you are at id N" frame), never a page handler.
		src.addEventListener("hello", (e) => {
			trackId(e);
			try {
				const d = JSON.parse(e.data);
				if (Number.isFinite(d.last_id) && d.last_id > lastId) lastId = d.last_id;
			} catch {
				/* data malformed: the id: line already seeded us */
			}
		});
		for (const name of names) {
			src.addEventListener(name, (e) => {
				trackId(e);
				let data = null;
				try {
					data = JSON.parse(e.data);
				} catch {
					return; // malformed payload: drop rather than guess
				}
				handlers[name](data);
			});
		}
		src.addEventListener("reset", (e) => {
			// Overflow / replay-gap recovery. The server jumped our id
			// to "now", so keep tracking it: if the stream later drops
			// and we reopen, ?since= must NOT ask for the pre-reset
			// range the refetch already covered.
			trackId(e);
			if (handlers.onReset) handlers.onReset();
		});
		// The error handler inspects its OWN instance (src), not the
		// es variable: if open() has since replaced es, a stale
		// stream's error must not arm a retry on the new stream's
		// behalf (the new instance gets its own handlers).
		src.onerror = () => {
			if (closed) return;
			// Transient failures retry natively (with Last-Event-ID).
			// Fatal conditions — notably the per-IP 429 cap — land the
			// stream in CLOSED; that needs our own delayed retry.
			if (src.readyState === EventSource.CLOSED) {
				reopenTimer = setTimeout(() => {
					reopenTimer = null;
					if (!closed && document.visibilityState === "visible") open();
				}, 15000);
			}
		};
	}

	function close() {
		closed = true;
		if (reopenTimer) clearTimeout(reopenTimer);
		if (es) es.close();
	}

	document.addEventListener("visibilitychange", () => {
		if (closed) return;
		if (document.visibilityState === "hidden") {
			if (es) es.close();
		} else if (!es || es.readyState === EventSource.CLOSED) {
			open(); // ?since=lastId asks the ring to bridge the gap
		}
	});

	// Entering bfcache (or navigating away): release the server's per-IP
	// slot deterministically. The browser would tear the socket down
	// while frozen anyway; the explicit close also lands readyState in
	// CLOSED, which the visibilitychange handler above relies on when
	// deciding whether a visible tab needs a reopen.
	window.addEventListener("pagehide", () => {
		if (closed) return;
		if (es) es.close();
	});

	// Leaving bfcache: pageshow with persisted=true. Always reopen —
	// open() closes whatever connection state is left first, and the
	// ?since=lastId bridge replays anything published during the
	// freeze, so this is lossless even if the browser somehow kept the
	// old stream half-alive. Non-persisted pageshow is a normal load:
	// boot's open() already ran and this must not double-connect.
	window.addEventListener("pageshow", (e) => {
		if (closed || !e.persisted) return;
		open();
	});

	open();
	return { close };
}
