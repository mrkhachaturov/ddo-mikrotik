package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config is the parsed environment for the sidecar. Fields are populated by
// Load() from MIKROTIK_* / WEBHOOK_* env vars. The sidecar is intentionally
// ownership-agnostic — it has no env var for any owner-identity concept.
// The caller (operator) stamps Labels["owner"] on every Endpoint and the
// sidecar persists/round-trips that value via the RouterOS `comment` field.
type Config struct {
	// Listen is the HTTP bind address. Default ":9090".
	Listen string

	// Address is the RouterOS API endpoint in host:port form. Required.
	// The port is taken from the user-supplied value verbatim; we do NOT
	// re-encode 8728 vs 8729 based on UseTLS — RouterOS deployments vary.
	Address string

	// Username / Password are resolved from one of the four mutually
	// exclusive sources (MIKROTIK_USERNAME, MIKROTIK_USERNAME_FILE, and the
	// password equivalent). Both are required.
	Username string
	Password string

	// UseTLS dials RouterOS over api-ssl (typically port 8729). When false,
	// the cleartext binary API (typically port 8728) is used.
	UseTLS bool
	// SkipTLSVerify disables certificate validation on the RouterOS API
	// connection. Only honoured when UseTLS=true. Off by default — opt-in.
	SkipTLSVerify bool

	// DefaultTTL is applied when an inbound Endpoint omits recordTTL
	// (RecordTTL <= 0). Stored as a Go int in seconds; the orchestrator
	// converts to the RouterOS duration string ("1h", "30m1s", etc.).
	DefaultTTL int

	// Zones is the set of zones this sidecar manages, returned verbatim on
	// GET /. Used to filter inbound creates/updates/deletes (FQDN must end
	// in one of them). Empty = no filter, every name is accepted.
	Zones []string
}

// Load populates a Config from the process environment. Returns an error if
// any required field is missing or any value fails validation.
func Load() (Config, error) {
	username, password, err := resolveCredentials()
	if err != nil {
		return Config{}, err
	}

	addr := strings.TrimSpace(os.Getenv("MIKROTIK_ADDRESS"))
	if addr == "" {
		return Config{}, errors.New("MIKROTIK_ADDRESS is required (host:port, e.g. 192.168.1.1:8728)")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return Config{}, fmt.Errorf("MIKROTIK_ADDRESS: %q is not a host:port pair: %w", addr, err)
	}

	defaultTTL, err := parsePositiveInt("MIKROTIK_DEFAULT_TTL", 3600)
	if err != nil {
		return Config{}, err
	}

	c := Config{
		Listen:        envOr("WEBHOOK_LISTEN", ":9090"),
		Address:       addr,
		Username:      username,
		Password:      password,
		UseTLS:        parseBool("MIKROTIK_USE_TLS"),
		SkipTLSVerify: parseBool("MIKROTIK_SKIP_TLS_VERIFY"),
		DefaultTTL:    defaultTTL,
		Zones:         parseZones(os.Getenv("MIKROTIK_ZONES")),
	}
	if !c.UseTLS && c.SkipTLSVerify {
		// Not an error — but the flag is meaningless without TLS, so warn
		// at boot via the caller. Encode it here by returning an explicit
		// resolved state (SkipTLSVerify stays true so /healthz can flag).
	}
	return c, nil
}

// resolveCredentials picks exactly one source per credential. Pairing rules:
//   - MIKROTIK_USERNAME XOR MIKROTIK_USERNAME_FILE
//   - MIKROTIK_PASSWORD XOR MIKROTIK_PASSWORD_FILE
//
// Both username and password must end up non-empty.
func resolveCredentials() (string, string, error) {
	user, err := pickSecret("MIKROTIK_USERNAME", "MIKROTIK_USERNAME_FILE")
	if err != nil {
		return "", "", err
	}
	pass, err := pickSecret("MIKROTIK_PASSWORD", "MIKROTIK_PASSWORD_FILE")
	if err != nil {
		return "", "", err
	}
	if user == "" {
		return "", "", errors.New("one of MIKROTIK_USERNAME or MIKROTIK_USERNAME_FILE is required")
	}
	if pass == "" {
		return "", "", errors.New("one of MIKROTIK_PASSWORD or MIKROTIK_PASSWORD_FILE is required")
	}
	return user, pass, nil
}

func pickSecret(envKey, fileKey string) (string, error) {
	env := os.Getenv(envKey)
	file := os.Getenv(fileKey)
	switch {
	case env != "" && file != "":
		return "", fmt.Errorf("set exactly one of %s or %s", envKey, fileKey)
	case env != "":
		return env, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s: read: %w", fileKey, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("%s: file %q is empty", fileKey, file)
		}
		return v, nil
	}
	return "", nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseBool(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

func parsePositiveInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// parseZones splits a comma-separated list, trims and lower-cases each
// entry, strips a trailing dot, and drops blanks. Empty input returns nil
// (no filter).
func parseZones(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		z := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p)), ".")
		if z == "" {
			continue
		}
		out = append(out, z)
	}
	return out
}
