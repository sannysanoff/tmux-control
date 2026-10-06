# tmux-control

Deterministic tmux <-> hermes session monitor with a REST API. Main identifier
is the hermes session id (`YYYYMMDD_HHMMSS_hexhex`).

No AI, no screen-content heuristics. All state comes from:

- tmux CLI (`list-panes`, `display-message`, `capture-pane`, `send-keys`)
- `/proc` (process aliveness, tty, suspended state from `stat` state field)
- `~/.hermes/runtime/active_sessions.json` (pid-keyed hermes lease file)
- `~/.hermes/terminal-sessions/tty-dev-pts-N` (per-tty current-session markers)
- `~/.hermes/state.db` (read-only sqlite3 CLI queries: sessions, last messages)
- `~/.hermes/logs/agent.log` (turn start/end lines -> running/answered/interrupted)

## Run

    ./tmux-control                          # local mode: listens on 127.0.0.1:8790
    ./tmux-control -mode local -listen :8790 -poll 2s -enter-delay 500ms
    ./tmux-control -mode spagetti -env .env # server mode: dials out to a gateway

Background: run under tmux/nohup; it is a daemon either way.

`-mode` selects how the same REST API is exposed:

- `local` (default) — a plain HTTP listener on `-listen`.
- `spagetti` — the daemon dials *out* to a spagetti gateway
  (https://github.com/sannysanoff/spagetti) and serves the API through an
  end-to-end encrypted channel, so no inbound port is needed. The local listener
  is not opened in this mode.

## Spagetti mode

Configuration comes from an env file (default `.env`, `-env` to point elsewhere)
and/or the process environment; the environment wins, and a file only fills gaps.
Three keys are required and a missing one refuses to start:

    SPAGETTI_GATEWAY=wss://mux.san.systems/ws   # gateway address (http/https map to ws/wss)
    SPAGETTI_PASSWORD=<server-role bearer token> # the "gateway password": who may register this node
    SPAGETTI_NAME=solidus-workspace              # node/service name clients ask the gateway for

The identity keypair and the access password are generated on the first run into
the working directory, using spagetti's own `conf` helpers:

    identity.key      private key, mode 0600 — never leaves this host
    identity.pub      public key (the pin)
    access.password   the Noise PSK (the pin), mode 0600

Copy `identity.pub` and `access.password` to a client (`tmux-web`, `spagetti-call pin`) to
authorise it; a second run reuses the files unchanged. Restart-safe and refuse-to-guess:
a malformed gateway URL, an empty required key or an unknown mode all fail at startup.

## Endpoints

- `GET /health` - liveness
- `GET /sessions` - all panes with associations; enriched with session row
  (title, profile, cwd, ended), last human query, last agent answer,
  turn state (running / answered / interrupted), and pane content (text array)
- `GET /sessions/{id}` - single pane status for one hermes session id
- `GET /sessions/{id}/pane` - current pane content (text array)
- `POST /sessions/{id}/break` - send Ctrl+C to the pane
- `POST /sessions/{id}/send` - inject text at the hermes input field
  (body: `{"text": "...", "enter": true}`; enters the text, waits
  `enter-delay`, presses Enter). Focus is verified deterministically: the
  viewport line at `cursor_y` must contain the hermes input glyph; otherwise
  HTTP error `pane not focused for input`.

Pane objects carry `aliases`: all tmux session names that reference the pane
(workspace groupings), so the same terminal reachable from multiple tmux
sessions/workspaces is one entry with several names.

## Association logic (deterministic)

1. tmux panes are enumerated with unique pane ids (`%N`).
2. Hermes processes are found in /proc by cmdline containing
   `.hermes/hermes-agent/`; state field `T` = suspended.
3. pid -> session from the hermes lease file (exact pid key).
4. Per-tty current-session markers override (they track /new and /resume).
5. Session title/profile/messages from state.db (read-only sqlite3).

## Status fields

- pane `status`: `running` (hermes fg alive) / `suspended-only` (all hermes
  procs SIGTSTP'd) / `no-hermes` / `dead-pane`
- `turn.state` from agent.log: `running` (turn started, not ended),
  `answered` (turn ended with text_response or other non-interrupt reason),
  `interrupted` (turn ended with interrupted_* reason); empty when the
  session had no turn in the recent log window.
