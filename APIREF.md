# tmux-control REST API reference

Base URL: `http://127.0.0.1:8790` (override with `-listen`).

All responses are JSON (`application/json`). Errors are
`{"error": "<message>"}` with an appropriate HTTP status
(400 bad request, 404 unknown session id, 409 wrong pane state,
500 tmux/sqlite failure).

Main identifier everywhere is the **hermes session id**:
`YYYYMMDD_HHMMSS_hexhex`, e.g. `20261005_124256_c6d046`. It matches the id in
`hermes sessions list` and `~/.hermes/state.db`.

## Endpoints

### GET /health

Liveness.

```json
{"now":"2026-10-05T17:01:18Z","ok":true,"version":"tmux-control 0.1"}
```

### GET /sessions

All tmux panes with their hermes associations. Fields:

| field | meaning |
|---|---|
| `pane` | tmux pane data: `pane_id` (`%N`, stable unique id), `tty`, `session` (tmux session name the pane was listed under), `aliases` (all tmux session names referencing the pane — workspaces), `win_index`, `window_name`, `pane_index`, `cmd` (foreground command name), `active`, `dead` |
| `has_hermes` | any hermes process attached to the pane tty |
| `running` | at least one hermes process in foreground (not SIGTSTP'd) |
| `status` | `running` / `suspended-only` / `no-hermes` / `dead-pane` |
| `associations` | hermes processes on the tty: `pid`, `running`, `suspended`, `session_id` (from hermes lease file), `profile` (the profile the process runs under, from its `-p`/`--profile` flag or its `HERMES_HOME`; absent for the default profile), `source` (`lease` when the lease entry matched this pid, `proc` when the process scan found it without a lease), `cmdline` |
| `session_id` | current session of the pane: per-tty marker file (tracks `/new`, `/resume`), falling back to the lease |
| `session` | row from `state.db`: `id`, `title`, `profile`, `cwd`, `ended`, `ended_at` |
| `profile` | the hermes profile the pane's process runs under (absent for the default profile); leases, markers, logs and the session row all come from that profile's own home, `<hermes-home>/profiles/<name>/` |
| `last_query` | latest human message: `role`, `content`, `ts` (ISO, from `state.db`) |
| `last_answer` | latest assistant message (same shape) |
| `turn` | last observed turn from `agent.log`: `state` (`running`/`answered`/`interrupted`), `since` (log timestamp), `detail` (turn-start query hint or turn-end reason, e.g. `interrupted_by_user`) |
| `exchange_state` | shortcut of `turn.state` |
| `pane_content` | current pane text as array of lines (fresh `tmux capture-pane`) |
| `rhermes_pid`, `rhermes_socket` | set when the pane runs under rhermes: the shim's pid and its control socket `/tmp/rhermes.<pid>` |
| `rhermes` | the shim's own status: `active_session` (runtime session id), `tui_attached`, `runtime_child`, `tui_pid`, `frames_up`, `frames_down`, `uptime_s`, `followed` |

Sessions with no turn in the recent agent.log window have `turn: null` — the
status line may still say the pane is running; the two are independent.

Query parameter `content=0` omits `pane_content` for every pane and runs no
tmux capture at all, which makes polling cheap. Any other value (or no
parameter) keeps the default behaviour.

Example (truncated):

```json
{
  "updated": "2026-10-05T14:05:26Z",
  "panes": [
    {
      "pane": {"pane_id":"%34","tty":"/dev/pts/16","session":"0",
               "aliases":["/dev/pts/0","/dev/pts/12","0","ws-test"],
               "win_index":"4","window_name":"mono-dev","pane_index":3,
               "cmd":"python","active":false,"dead":false},
      "has_hermes": true,
      "running": true,
      "status": "running",
      "session_id": "20261005_124256_c6d046",
      "session": {"id":"20261005_124256_c6d046",
                  "title":"Test tmux-control target",
                  "profile":"default","cwd":"/home/ubuntu/Fun/tmux-control",
                  "ended":false,"ended_at":""},
      "last_query": {"role":"user","content":"test tmux-control target",
                     "ts":"2026-10-05T12:43:20Z"},
      "last_answer": {"role":"assistant","content":"Operation interrupted: ...",
                      "ts":"2026-10-05T12:43:22Z"},
      "turn": {"state":"interrupted","since":"2026-10-05T14:04:45Z",
               "detail":"interrupted_by_user"},
      "exchange_state": "interrupted",
      "pane_content": [" ...pane lines... "]
    }
  ]
}
```

### GET /sessions/{id}

Single pane status for one hermes session id (same shape as one element of
`panes`, plus `turn`). 404 if the id is not associated with any pane.

### GET /sessions/{id}/pane

Current pane content.

```json
{"session_id":"20261005_124256_c6d046","pane_id":"%34","lines":["..."]}
```

### GET /sessions/{id}/messages

Recent conversation history for the session, from `state.db`.

Query parameter `limit` (optional): how many user/assistant messages to
return. Default 20, maximum 200 (higher values are clamped). A `limit` that
is not a positive integer, or is `<= 0`, is a 400
`{"error":"invalid limit: must be a positive integer"}`.

Query parameter `role` (optional): `user` or `assistant` returns only that
role's newest `limit` messages (chronological, oldest first, as below);
`all` or omitting the parameter keeps the default user+assistant mix.
Anything else (`tool`, empty-with-value, arbitrary text) is a 400
`{"error":"invalid role: must be user, assistant or all"}`.

Response (chronological, **oldest first** — the newest message is last; the
client reverses for its newest-first rail):

```json
{"session_id":"20261005_210044_b19833","count":4,
 "messages":[
  {"role":"user","content":"hello","ts":"2026-10-05T21:13:50Z"},
  {"role":"assistant","content":"Hello! ...","ts":"2026-10-05T21:13:53Z"}]}
```

`content` is full text, never truncated. Only `user` and `assistant` rows
are returned (tool/system rows skipped). A session with no messages is 200
with `count: 0` and `"messages": []`. 404 if the id is not associated with
any pane (same error text as `GET /sessions/{id}`).

### GET /sessions/{id}/events

The whole session, every role, in `id` order (oldest first) — the incremental
feed a client archives locally. Unlike `/messages`, tool rows and the tool calls
on assistant rows are included: the caller wants to know what was run and which
files were touched, not just what was said.

Query parameters (all optional):

| parameter | default | meaning |
|---|---|---|
| `since_id` | `0` | return rows with `id` greater than this. `0` starts at the beginning; otherwise pass the `last_id` of the previous page |
| `limit` | `500` | rows per page, clamped to 2000 |
| `max_chars` | `2000` | clip a tool row's output and every row's `tool_calls` to this many bytes (clamped to 200000). User/assistant text is never clipped below a 200000-byte safety cap |

```json
{"session_id":"20261006_200628_6e4365","count":3,"last_id":40350,"has_more":true,
 "events":[
  {"id":40348,"role":"user","content":"please add …","ts":"2026-10-06T18:08:52Z"},
  {"id":40349,"role":"assistant","tool_calls":"[{\"id\":…","reasoning":"the retry loop is where …","ts":"2026-10-06T18:08:55Z"},
  {"id":40350,"role":"tool","tool_name":"skill_view","content":"{\"success\": true…","ts":"2026-10-06T18:08:55Z"}]}
```

`has_more` is true when a full page came back, so a client pages until it is
short. Store `last_id` and ask for `since_id=<last_id>` next time: a session is
pulled once and only its new rows travel afterwards. Each row carries `id`,
`role`, `ts`, and then whichever of `content`, `tool_name`, `tool_calls`,
`reasoning` apply. `reasoning` is the agent's own thinking, on assistant rows
only, clipped by `max_chars` like a tool row — it is by far the largest column in
the table, and a reader wants its gist rather than its length. Clipping appends
`…` so a truncated field is never mistaken for the whole text.

The response is gzip-compressed when the request carries
`Accept-Encoding: gzip`; nothing lower in the stack compresses (the relay carries
ciphertext), so this is the only place a large page is squeezed.

### POST /sessions/{id}/break

Send Ctrl+C to the pane (interrupts the running turn). Body is ignored.

Success:

```json
{"action":"break","ok":true,"pane_id":"%44","session_id":"20261002_110720_8e4a84"}
```

409 if the pane has no foreground hermes (`session is not running in a pane`).

### POST /sessions/{id}/send

Inject text into the hermes input field via tmux, then press Enter.

Request:

```json
{"text":"run the tests and report", "enter": true}
```

- `text` (required) — the text to type.
- `enter` (optional, default `true`) — press Enter after typing. The daemon
  waits `enter-delay` (default 500 ms, `-enter-delay` flag) between typing and
  Enter so prompt_toolkit ingests the text.

Focus check (deterministic): the viewport line at tmux `cursor_y` must
contain the hermes input glyph `❯`; otherwise the request fails with 500
`pane not focused for input: cursor_y=... line=...`. This prevents typing
into a pane where the agent is streaming a response or the user is scrolled
into history.

Success:

```json
{"action":"send","focused":true,"input_row":28,"cursor":"2,28",
 "ok":true,"pane_id":"%34"}
```

409 if the pane has no foreground hermes.

### POST /sessions/{id}/paste

Insert text into the hermes input field as a **bracketed paste** (`tmux
paste-buffer -p`), so embedded newlines are inserted literally instead of
submitting a turn per line. Optionally press Enter afterwards.

Request (same shape as `/send`):

```json
{"text":"line one\nline two", "enter": true}
```

- `text` (required) — the text to paste.
- `enter` (optional, default `true`) — press Enter after the paste. As with
  `/send`, the daemon waits `enter-delay` (default 500 ms) between paste and
  Enter so prompt_toolkit ingests the text.

Mechanics: the text is delivered on stdin to `tmux load-buffer` under a
unique per-request buffer name (no argv length limit, no quoting), applied
with `tmux paste-buffer -p -t <pane>`, and the buffer is deleted afterwards.

Focus check is the same deterministic one `/send` performs: the viewport
line at tmux `cursor_y` must contain the hermes input glyph `❯`; otherwise
the request fails with 500. This prevents pasting into a pane where the
agent is streaming a response or the user is scrolled into history.

Success:

```json
{"action":"paste","bytes":19,"focused":true,"input_row":28,"cursor":"2,28",
 "ok":true,"pane_id":"%34"}
```

`bytes` is the number of bytes pasted. Errors match `/send`: 400 bad JSON or
missing text, 404 unknown session id, 409 no foreground hermes, 500 tmux
failure or failed focus check.

## rhermes

A pane that runs under [rhermes](rhermes) — the stock `hermes --tui` with its
runtime exposed on a unix control socket — is detected during the scan and
marked on the pane: `rhermes_pid`, `rhermes_socket` (`/tmp/rhermes.<pid>`) and
`rhermes` (the shim's own status: active runtime session, whether the TUI is
attached, frame counters, uptime). Such a session can be driven through the
socket, with no keystrokes pushed into the pane at all.

The flag `-rhermes-access` selects how `/send` and `/paste` behave for such a
session: `keystrokes` (the pane path as above), `socket` (the text becomes a
`prompt.submit` through the shim), or `auto` (default: socket when the pane
has a shim, keystrokes otherwise). A socket-routed answer carries
`"via":"rhermes"` so a caller can see which way it went.

Direct control, independent of that flag:

### GET /rhermes

Every live shim on this host: `instances` of `pid`, `socket`, and the shim's
`status` (or `note` when the socket did not answer).

### GET /sessions/{id}/rhermes/status

The shim's status for one session. 404 when the session's pane has no shim.

### GET /sessions/{id}/rhermes/frames?limit=N

The non-blocking pull. The daemon keeps one tap connection per shim (`follow`
on the ctl socket) and buffers every mirrored frame; this endpoint drains the
buffer — up to `limit` frames (default: all), oldest first, and the buffer
empties as it serves. Each frame is `{dir, line, at, clipped}`: `dir` is the
direction (`up` node→runtime, `down` runtime→node, `inj` an injected frame's
echo, `ctl` the follow ack), `line` the raw JSON frame, clipped to 8 KB with
`clipped:true`. `dropped` counts frames evicted from the full ring before this
drain.

### POST /sessions/{id}/rhermes/prompt

`{"text":"..."}` — inject a user turn through the shim (`prompt.submit`). The
reply arrives when the runtime accepted the turn (`"status":"streaming"`);
the answer itself is streamed on the wire, so it shows up in `/frames`.
Errors: 404 no shim on the pane, 502 the shim refused or timed out.

### POST /sessions/{id}/rhermes/send

`{"method":"...", "params":{...}}` — an arbitrary JSON-RPC call through the
shim to the runtime, resolved the same way as a prompt.

### POST /sessions/{id}/rhermes/stop

Tear the whole rhermes instance down — TUI, runtime and shim. Destructive; the
reply only says `{"stopped":true}`.

## Association logic (for reference)

Panes come from `tmux list-panes -a` keyed by unique pane id. Hermes
processes are discovered via `/proc` (cmdline contains
`.hermes/hermes-agent/`; state `T` = suspended; fd/0 gives the tty).
pid→session mapping comes from hermes' own lease file
(`~/.hermes/runtime/active_sessions.json`), overridden by the per-tty
current-session markers (`~/.hermes/terminal-sessions/tty-dev-pts-N`, which
track `/new` and `/resume`). Titles, profiles, and last messages are read
read-only from `~/.hermes/state.db` via the `sqlite3` CLI. Turn states are
parsed from `~/.hermes/logs/agent.log` turn-start/turn-end lines. No AI, no
screen-content interpretation anywhere.

## Daemon flags

| flag | default | meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8790` | HTTP listen address |
| `-poll` | `2s` | tmux/proc/lease scan interval |
| `-turn-window` | `20s` | (reserved) agent-log activity window |
| `-enter-delay` | `500ms` | pause between typing and Enter in `/send` |
| `-hermes-home` | `~/.hermes` | hermes home (`$HERMES_HOME` respected) |
| `-db` | `<hermes-home>/state.db` | path to hermes session store |
| `-capture-max` | `500` | max lines captured per pane |
| `-rhermes-access` | `auto` | how a rhermes-backed session is driven: `keystrokes`, `socket`, or `auto` (socket when the pane has a shim) |
