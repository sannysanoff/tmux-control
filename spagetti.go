package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sannysanoff/spagetti/conf"
	"github.com/sannysanoff/spagetti/server"
)

// Spagetti mode publishes the same REST API through a spagetti gateway instead
// of listening on a local port: the daemon dials out, so nothing has to be
// reachable inbound. Configuration comes from an env file (default .env) and/or
// the process environment; the identity keypair and the access password are
// generated on the first run into the working directory, using spagetti's own
// conf helpers, so a client can pin them.
const (
	// envGateway is the gateway address, e.g. wss://mux.san.systems/ws. http and
	// https are accepted and mapped onto ws and wss.
	envGateway = "SPAGETTI_GATEWAY"
	// envPassword is the gateway password: the server-role bearer token the
	// gateway accepts from this node. It is a door key — it decides who may ask
	// the gateway for a route, never who may read channel content.
	envPassword = "SPAGETTI_PASSWORD"
	// envName is the node/service name clients ask the gateway for. It is also
	// the label the daemon announces, so clients pin their channel to it.
	envName = "SPAGETTI_NAME"

	defaultEnvFile = ".env"
)

// spagettiKeys are the variables spagetti mode requires; anything else in the
// env file is ignored.
var spagettiKeys = []string{envGateway, envPassword, envName}

// runSpagetti publishes handler through a spagetti gateway until ctx is done.
// The identity and the access password live in the working directory as
// identity.key (private), identity.pub and access.password.
func runSpagetti(ctx context.Context, envFile string, handler http.Handler) error {
	vals, err := loadSpagettiEnv(envFile)
	if err != nil {
		return err
	}
	endpoint, err := gatewayEndpoint(vals[envGateway])
	if err != nil {
		return err
	}
	name := vals[envName]
	if err := validateServerName(name); err != nil {
		return err
	}

	// The same files a client pins: identity.pub (public key) and
	// access.password (the Noise PSK). identity.key never leaves this host.
	identity, idCreated, err := conf.LoadOrCreateIdentity(".")
	if err != nil {
		return err
	}
	password, pwCreated, err := conf.LoadOrCreatePassword(".")
	if err != nil {
		return err
	}
	if idCreated || pwCreated {
		log.Printf("spagetti: first run: wrote identity.key (private, mode 0600), identity.pub and access.password (mode 0600) into the working directory")
		log.Printf("spagetti: copy identity.pub and access.password to a client to pin server %q (fingerprint %s)", name, identity.Fingerprint())
	}
	log.Printf("spagetti: server %q (%s) -> %s (gateway password %d characters)", name, identity.Fingerprint(), endpoint, len(vals[envPassword]))

	return server.Serve(ctx, server.Options{
		ServerID:   name,
		Name:       name,
		GatewayURL: endpoint,
		Token:      vals[envPassword],
		Identity:   identity,
		Password:   password,
		Handler:    handler,
		Logf:       log.Printf,
	})
}

// loadSpagettiEnv resolves the required keys from the process environment
// first, then from the env file (each source filling only what is still
// missing), and refuses to start when one is absent from both — a daemon that
// starts with a guessed gateway looks like a network fault later on.
func loadSpagettiEnv(path string) (map[string]string, error) {
	vals := map[string]string{}
	origins := map[string]string{}
	for _, k := range spagettiKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			vals[k] = v
			origins[k] = "the environment"
		}
	}
	f, err := os.Open(path)
	switch {
	case err == nil:
		fileVals, perr := parseEnvFile(f, path)
		f.Close()
		if perr != nil {
			return nil, perr
		}
		for k, v := range fileVals {
			if _, have := vals[k]; have || v == "" {
				continue
			}
			vals[k] = v
			origins[k] = path
		}
	case os.IsNotExist(err):
		// The env file is optional; the process environment may carry everything.
	default:
		return nil, err
	}

	var missing []string
	for _, k := range spagettiKeys {
		if vals[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required %s; refused to start (consulted the process environment and %s)", strings.Join(missing, ", "), path)
	}
	for _, k := range spagettiKeys {
		if k == envPassword {
			log.Printf("spagetti: %s: %d characters (%s)", k, len(vals[k]), origins[k])
			continue
		}
		log.Printf("spagetti: %s: %s (%s)", k, vals[k], origins[k])
	}
	return vals, nil
}

// parseEnvFile reads KEY=VALUE lines, ignoring blanks and # comments. Keys are
// matched case-insensitively; a line that is not KEY=VALUE, or a value with an
// unterminated quote, is a misconfiguration and refuses to start.
func parseEnvFile(r io.Reader, path string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE, got %q", path, n, sc.Text())
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		val, err := envValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// envValue trims a value: a quoted value is taken verbatim up to its closing
// quote, an unquoted one loses a trailing " #" comment.
func envValue(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if q := v[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(v[1:], q)
		if end < 0 {
			return "", fmt.Errorf("unterminated %c quote", q)
		}
		return v[1 : 1+end], nil
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, nil
}

// gatewayEndpoint turns the configured address into the websocket URL the tunnel
// dials. Operators write the gateway's public URL, so http and https map onto ws
// and wss.
func gatewayEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", envGateway, raw, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("%s %q: scheme %q is not one of http, https, ws, wss", envGateway, raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s %q: no host", envGateway, raw)
	}
	return u.String(), nil
}

// validateServerName keeps the node/service name to what the gateway and a
// config directory can carry without surprises.
func validateServerName(name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", envName)
	}
	if len(name) > 128 {
		return fmt.Errorf("%s %q: longer than 128 characters", envName, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s %q: %q is not allowed; use letters, digits, dot, dash or underscore", envName, name, r)
		}
	}
	return nil
}
