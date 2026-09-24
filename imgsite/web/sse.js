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
//     (visibilitychange, fatal-error backoff) the id rides the URL as
//     ?since=N instead — the server's other replay entry point.
//   - visibilitychange: close when the tab hides, reopen when it
//     becomes visible. Frozen background tabs keep timers and sockets
//     alive only unpredictably; a deterministic close/reopen plus the
//     replay bridge is the documented approach.
//   - the "reset" event hook: the server sends it when this client's
//     buffer overflowed or its replay gap exceeded the ring —
//     full-state-refetch semantics, so the default handler reloads.
export function connect(handlers) {
	const names = Object.keys(handlers).filter((n) => n !== "onReset");
	let es = null;
	let lastId = 0;
	let closed = false;
	let reopenTimer = null;

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
		const url = lastId > 0 ? "/events?since=" + lastId : "/events";
		const src = new EventSource(url);
		es = src;
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

	open();
	return { close };
}
