// Package main: tmux-control — deterministic tmux <-> hermes session monitor
// with a REST API.
//
// The same API is exposed two ways, selected with -mode:
//   - local (default): a plain HTTP listener on -listen
//   - spagetti: the daemon dials out to a spagetti gateway and serves the API
//     through an end-to-end encrypted channel; configuration comes from an env
//     file (default .env) requiring SPAGETTI_GATEWAY, SPAGETTI_PASSWORD (the
//     gateway bearer token) and SPAGETTI_NAME, and the identity keypair plus
//     access password are generated into the working directory on first run.
//
// Sources (all deterministic):
//   - tmux: list-panes/display-message/capture-pane/send-keys via the tmux CLI
//   - /proc: process aliveness + tty mapping + hermes binary cmdline
//   - hermes lease file: ~/.hermes/runtime/active_sessions.json (pid -> session)
//   - hermes per-tty markers: ~/.hermes/terminal-sessions/tty-dev-pts-N
//     (current session id per tty, written on start//new//resume)
//   - hermes state.db (sqlite, read-only): session rows, message history
//   - hermes agent.log: per-session activity timestamps + turn outcomes
package main

const version = "tmux-control 0.1"

// Hermes message roles we treat as human queries and agent answers.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)
