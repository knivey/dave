package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- shared flow helpers -----------------------------------------------------

// The fabricated_urls / linkless_finals tests drive sessions whose every
// turn consumes exactly ONE scripted response (a no-tool final), so a
// turn-spec list plus a response list fully pins the session shape.

// turnSpec is one scripted turn: a user turn when user is set, a delivery
// turn (marker injected in s.cfg.deliveryRole) when delivery is set.
type turnSpec struct {
	user     string
	delivery string
}

func driveTurns(t *testing.T, s *session, steps []turnSpec) {
	t.Helper()
	for i, st := range steps {
		var err error
		if st.delivery != "" {
			err = s.deliveryTurn(st.delivery)
		} else {
			err = s.userTurn(st.user)
		}
		require.NoErrorf(t, err, "step %d", i)
	}
}

// newFlowSession builds a session around a scripted responder; delivery
// role stays the flag default ("system") unless a subtest overrides it.
func newFlowSession(t *testing.T, responses []string) *session {
	t.Helper()
	sc := &script{responses: responses}
	s, _ := newTestSession(t, sc, 1, "v", "imageflow")
	return s
}

// ---- leak_suspects -----------------------------------------------------------

// TestLeakSuspectDetector drives one-response sessions through the real
// request path: the line-anchored reasoning-label regex must catch the
// observed leak shape while leaving in-character "My thought:" prose alone.
func TestLeakSuspectDetector(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "label at content start", content: "thought: I will use the enhancement tool", want: 1},
		{name: "label on its own line after prose", content: "I shall weave this vision from the void.\n\nthought: I will use the enhancement tool to create a detailed prompt", want: 1},
		{name: "uppercase label", content: "THINKING: OUT LOUD", want: 1},
		{name: "mixed case reasoning", content: "Reasoning: the user wants a shrew", want: 1},
		{name: "indented label", content: "  reasoning: quietly, in the margins", want: 1},
		{name: "space before colon", content: "thought : the plan", want: 1},

		{name: "My thought at line start", content: "My thought: the portrait needs work", want: 0},
		{name: "inline thinking mid-sentence", content: "as I said, my thinking: works differently here", want: 0},
		{name: "inline reasoning mid-sentence", content: "the reasoning: label is not at a line start", want: 0},
		{name: "plural label", content: "thoughts: many and scattered", want: 0},
		{name: "label without colon", content: "thought I would try again", want: 0},
		{name: "empty content", content: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &script{responses: []string{cannedResp(tt.content, "")}}
			s, _ := newTestSession(t, sc, 1, "v", "imageflow")
			require.NoError(t, s.userTurn("hi"))
			assert.Equal(t, tt.want, s.leakSuspects)
		})
	}
}

// ---- template_leaks ----------------------------------------------------------

// TestTemplateLeakDetector drives one-response sessions through the real
// request path: the gemma4 special-token literals must trip while ordinary
// angle-bracket text, prose words, and IRC formatting codes never match.
func TestTemplateLeakDetector(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		// positives — the two OBSERVED shapes first (run-02's bare stub,
		// run-05's raw tool-call syntax mid-prose), then the rest of the
		// gemma4 token family
		{name: "run-02 bare channel stub", content: "<channel|>", want: 1},
		{name: "run-05 tool_call syntax mid-prose", content: "behold, your portrait! <|tool_call>call:enhance_and_generate_async{prompt:<|\"|>a shrew<|\"|>, negative_prompt:<|\"|>none<|\"|>}", want: 1},
		{name: "tool_response token", content: "the tool answers <|tool_response>\nwith an image", want: 1},
		{name: "channel open token", content: "she said <|channel>final and left", want: 1},
		{name: "turn tokens", content: "<|turn>system<turn|>", want: 1},
		{name: "think token", content: "quietly, <|think>about shrews", want: 1},
		{name: "image token", content: "see <|image|> for the portrait", want: 1},

		{name: "html angle brackets", content: "<div>the portrait</div>", want: 0},
		{name: "channel as a prose word", content: "turn the channel over, we must talk", want: 0},
		{name: "turn as a prose word", content: "wait your turn, little shrew", want: 0},
		{name: "unclosed bracket without pipe", content: "we <turn toward the falcon", want: 0},
		{name: "irc color and bold codes", content: "\x034behold\x03 a \x02bold\x02 portrait", want: 0},
		{name: "empty content", content: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &script{responses: []string{cannedResp(tt.content, "")}}
			s, _ := newTestSession(t, sc, 1, "v", "imageflow")
			require.NoError(t, s.userTurn("hi"))
			assert.Equal(t, tt.want, s.templateLeaks)
		})
	}

	t.Run("one response counts once regardless of hit count", func(t *testing.T) {
		sc := &script{responses: []string{cannedResp("<channel|> and <|turn> and <|image|>", "")}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("hi"))
		assert.Equal(t, 1, s.templateLeaks)
	})

	t.Run("multiple responses count individually", func(t *testing.T) {
		sc := &script{responses: []string{
			cannedResp("one moment <channel|>", ""),
			cannedResp("done <|turn>", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("hi"))
		require.NoError(t, s.userTurn("again"))
		assert.Equal(t, 2, s.templateLeaks)
	})

	t.Run("reasoning_content carrying channel tokens does not count", func(t *testing.T) {
		// Content-ONLY detector: channel-shaped thinking landing in
		// reasoning_content is the model doing its job internally, never
		// a user-visible leak.
		sc := &script{responses: []string{cannedResp("your portrait, as requested", "<|channel>thought\nI will enhance the prompt.")}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("hi"))
		assert.Equal(t, 0, s.templateLeaks)
	})
}

// ---- fabricated_urls ---------------------------------------------------------

func TestFabricatedURLsTiming(t *testing.T) {
	const url1 = "https://i.shrews.xyz/AA0001"
	const url2 = "https://i.shrews.xyz/BB0002"
	const forged = "https://i.shrews.xyz/FF0001"
	marker1 := deliveryMarker("11111111", url1)
	marker2 := deliveryMarker("22222222", url2)

	t.Run("url before any delivery counts", func(t *testing.T) {
		s := newFlowSession(t, []string{cannedResp("behold "+url1, "")})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}})
		assert.Equal(t, 1, s.fabricatedURLs, "an image-site URL cited before any delivery is undelivered")
	})

	t.Run("url matching the just-delivered set does not count", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("here it is: "+url1, ""),
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}})
		assert.Equal(t, 0, s.fabricatedURLs)
	})

	t.Run("delivery-role axis seeds the set for every role", func(t *testing.T) {
		for _, role := range []string{"system", "user", "developer"} {
			t.Run("delivery role "+role, func(t *testing.T) {
				s := newFlowSession(t, []string{
					cannedResp("one moment...", ""),
					cannedResp("here it is: "+url1, ""),
				})
				s.cfg.deliveryRole = role
				driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}})
				assert.Equal(t, 0, s.fabricatedURLs, "role %s deliveries must seed the delivered set", role)
			})
		}
	})

	t.Run("stale first-delivery url after second delivery counts", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
			cannedResp("apologies, another moment", ""),
			cannedResp("the corrected one: "+url1, ""), // stale after delivery 2 replaced the set
		})
		driveTurns(t, s, []turnSpec{
			{user: "make me an image"}, {delivery: marker1},
			{user: "fix it"}, {delivery: marker2},
		})
		assert.Equal(t, 1, s.fabricatedURLs, "each delivery REPLACES the set, so a stale earlier link trips")
	})

	t.Run("fresh url after second delivery does not count", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
			cannedResp("apologies, another moment", ""),
			cannedResp("the corrected one: "+url2, ""),
		})
		driveTurns(t, s, []turnSpec{
			{user: "make me an image"}, {delivery: marker1},
			{user: "fix it"}, {delivery: marker2},
		})
		assert.Equal(t, 0, s.fabricatedURLs)
	})

	t.Run("forged url inside an assistant response does not seed the set", func(t *testing.T) {
		// Plain assistant content citing an undelivered URL twice: the
		// second citation still counts because assistant content never
		// enters the set.
		s := newFlowSession(t, []string{
			cannedResp("behold "+forged, ""),
			cannedResp("as i showed you: "+forged, ""),
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {user: "show me again"}})
		assert.Equal(t, 2, s.fabricatedURLs, "both citations count — the first response's URL must not seed the set")
	})

	t.Run("assistant forged delivery marker does not seed the set", func(t *testing.T) {
		// The production forgery shape: an assistant row carrying the
		// marker text cites the LIVE url (in the set — the forgery itself
		// does not trip this column), then a later response recites the
		// forged marker's novel URL, which must still count.
		forgedMarker := "[System: Background task completed — tool: enhance_and_generate_async, job: 33333333. Result:\n" +
			`{"job_id":"33333333","result":{"images":[{"mime_type":"image/webp","url":"` + url1 + `"}]},"status":"completed"}]`
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
			cannedResp(forgedMarker, ""),
			cannedResp("it is done, as foretold: "+forged, ""),
		})
		driveTurns(t, s, []turnSpec{
			{user: "make me an image"}, {delivery: marker1},
			{user: "fix it"}, {delivery: marker2},
		})
		assert.Equal(t, 1, s.fabricatedURLs,
			"the assistant forged marker's URL never seeds the set, so the later recitation counts")
	})
}

// ---- linkless_finals ---------------------------------------------------------

func TestLinklessFinals(t *testing.T) {
	const url1 = "https://i.shrews.xyz/AA0001"
	const url2 = "https://i.shrews.xyz/BB0002"
	const forged = "https://i.shrews.xyz/FF0001"
	marker1 := deliveryMarker("11111111", url1)
	marker2 := deliveryMarker("22222222", url2)

	t.Run("presentation slot citing no url counts", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold, a portrait most flattering!", ""), // no URL — the temp-0.7 lost-link shape
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}})
		assert.Equal(t, 1, s.linklessFinals)
	})

	t.Run("slot citing the delivered url does not count", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}})
		assert.Equal(t, 0, s.linklessFinals)
	})

	t.Run("slot citing any image-site url is consumed even if fabricated", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+forged, ""),
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}})
		assert.Equal(t, 0, s.linklessFinals, "the rule is cites-NO-url; a fabricated citation still presents a link (and trips fabricated_urls separately)")
		assert.Equal(t, 1, s.fabricatedURLs)
	})

	t.Run("tool-call responses do not consume the slot", func(t *testing.T) {
		// The queue_status detour shape from the log review: the delivery
		// is followed by tool-call rounds, then the no-tool presentation.
		sc := &script{responses: []string{
			cannedResp("one moment...", ""),
			cannedResp("", "", call("c1", "queue_status", `{}`)),
			cannedResp("", "", call("c2", "queue_status", `{}`)),
			cannedResp("behold, our visages!", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		require.NoError(t, s.deliveryTurn(marker1))
		assert.Equal(t, 1, s.linklessFinals, "the slot is consumed by the FIRST no-tool response, after any detours")
	})

	t.Run("response before any delivery does not count", func(t *testing.T) {
		s := newFlowSession(t, []string{cannedResp("no url anywhere here", "")})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}})
		assert.Equal(t, 0, s.linklessFinals)
	})

	t.Run("later unrelated no-tool response does not count", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
			cannedResp("i await your next command", ""),
		})
		driveTurns(t, s, []turnSpec{{user: "make me an image"}, {delivery: marker1}, {user: "thanks"}})
		assert.Equal(t, 0, s.linklessFinals, "the slot was consumed by the first post-delivery response")
	})

	t.Run("each delivery arms a fresh slot", func(t *testing.T) {
		s := newFlowSession(t, []string{
			cannedResp("one moment...", ""),
			cannedResp("behold: "+url1, ""),
			cannedResp("another moment...", ""),
			cannedResp("the corrected portrait, at last! (no link, imagine it)", ""),
		})
		driveTurns(t, s, []turnSpec{
			{user: "make me an image"}, {delivery: marker1},
			{user: "fix it"}, {delivery: marker2},
		})
		assert.Equal(t, 1, s.linklessFinals, "only the second delivery's slot goes linkless")
	})
}

// ---- silent_turns ------------------------------------------------------------

func TestSilentTurns(t *testing.T) {
	t.Run("empty no-tool response counts", func(t *testing.T) {
		sc := &script{responses: []string{cannedResp("", "")}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("hi"))
		assert.Equal(t, 1, s.silentTurns)
	})

	t.Run("whitespace-only counts", func(t *testing.T) {
		sc := &script{responses: []string{cannedResp("\n  ", "")}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("hi"))
		assert.Equal(t, 1, s.silentTurns)
	})

	t.Run("empty content with tool calls does not count", func(t *testing.T) {
		// The silent job-submission shape: empty content, but the turn is
		// doing tool work — normal agentic behavior, not a silent turn.
		sc := &script{responses: []string{
			cannedResp("", "", call("c1", "enhance_and_generate_async", `{}`)),
			cannedResp("one moment, my liege", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		assert.Equal(t, 0, s.silentTurns)
	})

	t.Run("mixed session counts only the blank no-tool final", func(t *testing.T) {
		sc := &script{responses: []string{
			cannedResp("", "", call("c1", "generate_image_async", `{}`)), // silent submit, tool call
			cannedResp("\n", ""), // the silent final
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		assert.Equal(t, 1, s.silentTurns, "the empty tool-call round is not silent; the blank final is")
	})
}

// ---- jobs / registers --------------------------------------------------------

func TestJobsAndRegistersCounted(t *testing.T) {
	sc := &script{responses: []string{
		// turn 1: enhance + register in ONE response
		cannedResp("", "",
			call("c1", "enhance_and_generate_async", `{}`),
			call("c2", "register_background_job", `{}`)),
		cannedResp("one moment, my liege", ""),
		// turn 2: generate + enhance in one response
		cannedResp("", "",
			call("c3", "generate_image_async", `{}`),
			call("c4", "enhance_and_generate_async", `{}`)),
		cannedResp("another is underway", ""),
		// turn 3: non-job detours must not count
		cannedResp("", "", call("c5", "queue_status", `{}`)),
		cannedResp("", "", call("c6", "brave_web_search", `{"query":"c64"}`)),
		cannedResp("still working on it", ""),
	}}
	s, _ := newTestSession(t, sc, 1, "v", "imageflow")
	require.NoError(t, s.userTurn("make me an image"))
	require.NoError(t, s.userTurn("another, corrected"))
	require.NoError(t, s.userTurn("status?"))
	assert.Equal(t, 3, s.jobs, "enhance + generate + enhance across the session")
	assert.Equal(t, 1, s.registers, "the single register_background_job call")
}

// ---- orphan_jobs -------------------------------------------------------------

// TestOrphanJobs drives scripted sessions whose register calls cite the
// REAL fabricated ids: drawIDs pre-draws them from an identically-seeded
// rng (same key newTestSession uses — job ids are the first draws of the
// session stream, one per submission in call order). orphanJobCount is
// invoked the way runOne invokes it, at session end — registers only ever
// arrive inside responses, so nothing can register after the final one.
func TestOrphanJobs(t *testing.T) {
	drawIDs := func(n int) []string {
		rng := seededRng(1, "v", "imageflow", "1")
		ids := make([]string, n)
		for i := range ids {
			ids[i] = randomJobID(rng)
		}
		return ids
	}
	submit := func(id, name string) wireToolCall { return call(id, name, `{}`) }
	register := func(id string) wireToolCall {
		return call("r"+id, registerJobTool, `{"job_id":"`+id+`","tool_name":"enhance_and_generate_async"}`)
	}

	t.Run("normal run, every submission registered", func(t *testing.T) {
		ids := drawIDs(2)
		sc := &script{responses: []string{
			cannedResp("", "", submit("c1", "enhance_and_generate_async")),
			cannedResp("", "", register(ids[0])),
			cannedResp("one moment, my liege", ""),
			cannedResp("", "", submit("c2", "generate_image_async")),
			cannedResp("", "", register(ids[1])),
			cannedResp("another is underway", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		require.NoError(t, s.userTurn("another"))
		assert.Equal(t, 2, s.jobs)
		assert.Equal(t, 2, s.registers)
		assert.Equal(t, 0, s.orphanJobCount())
	})

	t.Run("run-05 shape: five submissions, four distinct registers, one duplicate", func(t *testing.T) {
		ids := drawIDs(5)
		sc := &script{responses: []string{
			cannedResp("", "",
				submit("c1", "enhance_and_generate_async"),
				submit("c2", "enhance_and_generate_async")), // submissions 1, 2
			cannedResp("", "", register(ids[0]), register(ids[1])),
			cannedResp("", "",
				submit("c3", "enhance_and_generate_async"),
				submit("c4", "enhance_and_generate_async")), // submissions 3, 4
			cannedResp("", "", register(ids[2]), register(ids[3])),
			cannedResp("", "", submit("c5", "enhance_and_generate_async")), // submission 5 — the 6bc8bcba shape
			cannedResp("", "", register(ids[2])),                           // the redundant duplicate register
			cannedResp("five portraits are underway, my liege", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me a burst of images"))
		assert.Equal(t, 5, s.jobs)
		assert.Equal(t, 5, s.registers, "four distinct cites plus one duplicate")
		assert.Equal(t, 1, s.orphanJobCount(), "only the never-registered fifth submission is orphaned; the duplicate rescues nothing new and double-counts nothing")
	})

	t.Run("register for an unsubmitted id rescues nothing", func(t *testing.T) {
		ids := drawIDs(1)
		const phantom = "00000000"
		require.NotEqual(t, phantom, ids[0], "fixture hygiene: the phantom id must differ from the fabricated one")
		sc := &script{responses: []string{
			cannedResp("", "", submit("c1", "enhance_and_generate_async")),
			cannedResp("", "", register(phantom)),
			cannedResp("one moment, my liege", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		assert.Equal(t, 1, s.registers)
		assert.Equal(t, 1, s.orphanJobCount(), "the real submission stays orphaned; the phantom register is a different anomaly, not counted")
	})

	t.Run("register with no submissions orphans nothing", func(t *testing.T) {
		sc := &script{responses: []string{
			cannedResp("", "", register("00000000")),
			cannedResp("one moment, my liege", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		assert.Equal(t, 0, s.jobs)
		assert.Equal(t, 0, s.orphanJobCount())
	})

	t.Run("register arriving turns later still rescues at session end", func(t *testing.T) {
		ids := drawIDs(1)
		sc := &script{responses: []string{
			cannedResp("", "", submit("c1", "enhance_and_generate_async")),
			cannedResp("one moment, my liege", ""), // turn 1 ends UNREGISTERED
			cannedResp("", "", register(ids[0])),   // the rescue arrives a whole turn later
			cannedResp("it is now being watched", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "v", "imageflow")
		require.NoError(t, s.userTurn("make me an image"))
		require.NoError(t, s.userTurn("actually, watch it"))
		assert.Equal(t, 0, s.orphanJobCount(), "the registered set accumulates to session end — a late register rescues an early submission")
	})

	t.Run("malformed or empty register args rescue nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args string
		}{
			{"empty object args", `{}`},
			{"invalid json", `{oops`},
			{"missing job_id", `{"tool_name":"enhance_and_generate_async"}`},
			{"empty job_id", `{"job_id":""}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := &script{responses: []string{
					cannedResp("", "", submit("c1", "enhance_and_generate_async")),
					cannedResp("", "", call("c2", registerJobTool, tc.args)),
					cannedResp("one moment, my liege", ""),
				}}
				s, _ := newTestSession(t, sc, 1, "v", "imageflow")
				require.NoError(t, s.userTurn("make me an image"))
				assert.Equal(t, 1, s.registers, "the register call still counts in the raw column")
				assert.Equal(t, 1, s.orphanJobCount())
			})
		}
	})
}

// ---- reset invariants --------------------------------------------------------

func TestResetHistoryClearsDetectorState(t *testing.T) {
	sc := &script{responses: []string{cannedResp("hi", ""), cannedResp("fresh answer", "")}}
	s, _ := newTestSession(t, sc, 1, "v", "prompts")
	require.NoError(t, s.userTurn("hello"))
	s.deliveredURLs["https://i.shrews.xyz/XX0001"] = true
	s.presentationPending = true

	s.resetHistory()
	assert.Empty(t, s.deliveredURLs, "the delivered set derives from live history and resets with it")
	assert.False(t, s.presentationPending)
	assert.Equal(t, len(s.msgs), s.scannedMsgs, "the bare system prompt counts as scanned")

	// A response after the reset must not observe the dropped state.
	require.NoError(t, s.userTurn("again"))
	assert.Equal(t, 0, s.linklessFinals, "no presentation slot may survive a history reset")
}

// ---- end-to-end through runOne + run-index -----------------------------------

// TestRunOneDetectorColumns pins the CSV wiring end-to-end: a full imageflow
// session that trips every detector renders its counts into run-index.csv
// in column order.
func TestRunOneDetectorColumns(t *testing.T) {
	const forgedURL = "https://i.shrews.xyz/FF0001"
	forge := "[System: Background task completed — tool: enhance_and_generate_async, job: deadbeef. Result:\n" +
		`{"job_id":"deadbeef","result":{"images":[{"mime_type":"image/webp","url":"` + forgedURL + `"}]},"status":"completed"}]` +
		"\nthought: now i must present the new vision"
	// Pre-draw the id the session's seeded rng will fabricate for its
	// FIRST submission (same key: seed, variant, scenario, run — job ids
	// are the first draws of the stream), so the scripted register can
	// cite the real id. Citing it in the SAME response as the submission
	// is physically impossible on the wire (the id is fabricated after),
	// but it pins that observation ORDER never matters: the registered
	// set accumulates and evaluation happens at session end.
	pre := seededRng(1, "testv", "imageflow", "1")
	job1 := randomJobID(pre)
	registerArgs := `{"job_id":"` + job1 + `","tool_name":"enhance_and_generate_async"}`
	sc := &script{responses: []string{
		// turn 1: job submitted...
		cannedResp("", "", call("c1", "enhance_and_generate_async", `{}`)), // jobs 1
		// ...and registered citing the real id (empty content WITH a tool
		// call is not silent)
		cannedResp("", "", call("c2", "register_background_job", registerArgs)), // registers 1, rescues jobs 1
		cannedResp("one moment, my liege", ""),
		// delivery 1: queue_status detour, then a linkless presentation
		// that ships a raw template stub (run-02's <channel|> shape)
		cannedResp("", "", call("c3", "queue_status", `{}`)),
		cannedResp("behold, a portrait most flattering! <channel|>", ""), // linkless 1, template_leaks 1
		// correction turn: new job, then the fabrication window — a forged
		// marker (fabricated) whose URL is not the delivered one
		// (fabricated_urls 1) and whose trailing line leaks the label
		// (leak_suspects 1)
		cannedResp("", "", call("c4", "generate_image_async", `{}`)), // jobs 2 — never registered: orphan 1
		cannedResp(forge, ""),
		// delivery 2: an empty final — linkless 2 AND silent 1
		cannedResp("", ""),
	}}
	cfg := runConfig{model: "test-model", tools: advertisedTools, seed: 1, deliveryRole: "system"}
	outdir := t.TempDir()
	row, err := runOne(loadedVariant{name: "testv", text: "SYS PROMPT"}, scenarioImageflow, 1, cfg, sc.responder(), outdir)
	require.NoError(t, err)
	require.NotNil(t, row)

	assert.Equal(t, 8, row.Requests, "3 (turn 1) + 2 (delivery 1 + detour) + 2 (correction) + 1 (delivery 2)")
	assert.Equal(t, 8, row.Responses)
	assert.True(t, row.Fabricated, "the forged marker keeps tripping the prefix detector")
	assert.Equal(t, 1, row.LeakSuspects)
	assert.Equal(t, 1, row.TemplateLeaks)
	assert.Equal(t, 1, row.FabricatedURLs)
	assert.Equal(t, 2, row.LinklessFinals)
	assert.Equal(t, 1, row.SilentTurns)
	assert.Equal(t, 2, row.Jobs)
	assert.Equal(t, 1, row.Registers)
	assert.Equal(t, 1, row.OrphanJobs, "the second submission was never registered; the first was rescued by the cited id")

	var buf bytes.Buffer
	idx := newRunIndex(&buf)
	require.NoError(t, idx.add(*row))
	require.NoError(t, idx.close())
	want := "variant,tool_variant,temperature,retries,scenario,delivery_role,run,requests,responses,reasoning_chars_total,fabricated,leak_suspects,template_leaks,fabricated_urls,linkless_finals,silent_turns,jobs,registers,orphan_jobs\n" +
		"testv,default,default,0,imageflow,system,01,8,8,0,true,1,1,1,2,1,2,1,1\n"
	assert.Equal(t, want, buf.String())

	// The JSONL log itself is untouched by detection: strictly alternating
	// request/response records, one pair per exchange.
	lines := mustReadLines(t, filepath.Join(outdir, "testv", "imageflow-system", "run-01.jsonl"))
	assert.Len(t, lines, 16, "8 requests + 8 responses")
}
