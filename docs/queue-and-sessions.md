# Queue & Session System Design

## Commands and Sessions

**Chat commands** (triggered by prefix like `=grk`, `-chat`, etc.) always create a brand new conversation session. The previous session for that user on that channel is completed and a fresh one starts.

**Continuations** (addressing the bot by nick, e.g. `dave_bird, tell me more`) resume the active session. If no active session exists, the bot says "no context" and ignores it.

## Queue and Delivery

All command execution goes through the queue. Responses are delivered to IRC one at a time, in the order commands were received, per channel. If output is currently being sent for any user on that channel, the new command waits its turn.

When a command has to wait, the user sees **"⏳ Queued (position N)"** — N reflects how many outputs are ahead of theirs in the delivery order for that channel, regardless of who triggered them.

When it's the user's turn, they see **"▶ Processing your request (waited Xs)..."** before the response. For image generation, this becomes **"▶ Processed your image request (waited Xs)... (prompt)"**.

## Parallel Execution

The parallel setting controls how many commands can hit the AI API simultaneously. It does not change delivery order. With parallel enabled, commands from different users (or the same user) start their API calls at the same time, but their output still lines up and delivers one after another.

## Model gating (llama services)

`parallel` is per **service** and knows nothing about which **model** a
command uses. On a local llama-server router that can only hold one
model in memory at a time, two commands requesting different models
would otherwise trigger a mid-generation model swap (the server unloads
the running model, serves the new request, reloads, and re-prefills the
first — a long stall plus wasted work).

The model gate serializes whole turns across **distinct models** on the
same server, while same-model turns keep running concurrently. Capacity
is auto-detected from the server (`GET /props`, `max_instances`); a
service-level `models_max` can force it (see services.toml).

When a command is queued, the queue notice says on the same line whether
the request is already processing in the background (it started
immediately — its service had a free `parallel` slot) and whether a
model swap is predicted for it —
e.g. `⏳ Queued (position 1) — already processing in the background,
gemma will load after qwen finishes`.

A request in an otherwise-idle channel never queues, but may still not
start generating right away — all execution slots busy with other
channels' requests, or a model swap predicted. In those cases it gets
one immediate line saying why (`waiting for a free slot…` / `{model} is
waiting for {held} to finish…`); requests that start free stay silent.
A safety valve aborts gate waits that exceed 10 minutes / 3× the
command timeout.

## Async Background Tasks (Image Generation)

Some tool calls (like image generation) run in the background. The AI decides to use the tool, the job is submitted to the image server, and the AI's response finishes normally. The image job runs independently.

When the image job completes:

1. The bot queues a delivery job in the same channel queue
2. If the user has an active session that's *different* from the one that started the image, the bot **switches back** to the original session (completing whatever newer session the user may have started), delivers the image result into that session, and runs an AI turn so the AI can comment on the image
3. The user sees a **"Switched to session X"** notice when this happens
4. If the user's active session is already the right one, no switch needed — the result is just injected and the AI responds

This means a user can do: `=grk draw me a sunset` → AI says "generating..." → user does `=grk tell me a joke` → joke response → image finishes → bot switches back to the image session, shows the image, AI comments on it. The user can then continue the conversation about the image.
