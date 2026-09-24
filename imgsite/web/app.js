// imgsite module entry: wires page behavior by <body data-page>.
// The server renders complete HTML for every state; this layer only
// augments (infinite scroll, thumb swap, timestamp localization, SSE,
// search).
"use strict";

const page = document.body.dataset.page;
if (page === "gallery") {
	// search.boot() runs BEFORE gallery.boot(): on a server-rendered
	// /search?q= page it must install the query-scoped fragment URL
	// provider and activate the SSE filter before gallery.js observes
	// the results sentinel — otherwise the first scroll would fetch the
	// unfiltered /gallery fragment.
	Promise.all([import("./gallery.js"), import("./search.js")])
		.then(([gallery, search]) => {
			search.boot();
			gallery.boot();
		})
		.catch((err) => console.error("gallery page modules failed to load", err));
} else if (page === "image") {
	import("./image.js").then((m) => m.boot()).catch((err) =>
		console.error("image module failed to load", err)
	);
}
