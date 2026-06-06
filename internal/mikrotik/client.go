// Package mikrotik wraps the RouterOS native binary API
// (github.com/go-routeros/routeros/v3) behind a small interface
// (Client) so the orchestrator can be tested with a fake. Conversion
// between the wire (FQDN strings, integer TTL seconds) and RouterOS
// (duration strings, mx-preference/mx-exchange fields, .id row IDs) lives
// here and only here.
package mikrotik

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	routeros "github.com/go-routeros/routeros/v3"
)

// Record models a single /ip/dns/static row at rest on the router.
// Fields are populated from the RouterOS reply Map verbatim — name and
// comment are passed back through to the orchestrator unmodified so that
// the operator's Labels["owner"] round-trips byte-for-byte.
type Record struct {
	// ID is the RouterOS row identifier (".id" — e.g. "*1A"). Required
	// for /set and /remove; opaque to the orchestrator.
	ID string

	// Name is the FQDN (no trailing dot) as RouterOS stores it. Empty for a
	// wildcard row, which is keyed by Regexp instead (RouterOS treats a
	// literal asterisk in `name` as a dead non-matching character). Exactly
	// one of Name / Regexp is set on any row this sidecar writes.
	Name string

	// Regexp is the RouterOS `regexp` field, set only for DNS wildcard rows
	// ("*.dev.example.com"). It holds the one fixed, reversible shape this
	// sidecar writes — see wildcard.go (WildcardToRegexp / RegexpToWildcard).
	// On read, only rows whose regexp matches that shape AND carry our
	// ownership Comment are surfaced; arbitrary user-authored regexp rows are
	// skipped. When Regexp is set, Name is empty (and vice versa).
	Regexp string

	// Type is one of "A", "AAAA", "CNAME", "MX", "NS". RouterOS defaults
	// the field to "A" on add-without-type, so we always send an explicit
	// =type=… and read the field with the same expectation.
	Type string

	// TTL is the RouterOS duration string ("1h", "30m1s", "1d", …). Empty
	// when the record was added with no TTL (RouterOS then falls back to
	// the global cache TTL, which we don't surface).
	TTL string

	// Comment is the ownership bridge — we persist Labels["owner"] here
	// verbatim and read it back the same way.
	Comment string

	// Type-specific rdata:
	Address string // A / AAAA
	CNAME   string // CNAME
	NS      string // NS
	MXHost  string // MX exchange
	MXPref  int    // MX preference (1..65535)
}

// Client is the surface the orchestrator depends on. All methods take a
// context for cancellation and return wrapped errors that include the
// operation. Concrete implementation lives in *APIClient; tests use a
// fake.
type Client interface {
	List(ctx context.Context) ([]Record, error)
	Add(ctx context.Context, r Record) (string, error)
	Set(ctx context.Context, id string, r Record) error
	Remove(ctx context.Context, id string) error
	Ping(ctx context.Context) error
	Close() error
}

// APIClient is the production Client backed by a go-routeros/v3 connection.
// Connections are dialled lazily on the first call and cached for reuse;
// a failed call closes the cached connection so the next call re-dials.
type APIClient struct {
	address       string
	username      string
	password      string
	useTLS        bool
	skipTLSVerify bool
	dialTimeout   time.Duration

	mu   sync.Mutex
	conn *routeros.Client
}

// NewAPIClient constructs an APIClient. No I/O happens here — the
// underlying connection is established lazily by ensureConn().
func NewAPIClient(address, username, password string, useTLS, skipTLSVerify bool) *APIClient {
	return &APIClient{
		address:       address,
		username:      username,
		password:      password,
		useTLS:        useTLS,
		skipTLSVerify: skipTLSVerify,
		dialTimeout:   15 * time.Second,
	}
}

func (c *APIClient) ensureConn(ctx context.Context) (*routeros.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	var (
		cl  *routeros.Client
		err error
	)
	if c.useTLS {
		tlsCfg := &tls.Config{InsecureSkipVerify: c.skipTLSVerify} //nolint:gosec // controlled by config
		cl, err = routeros.DialTLSContext(ctx, c.address, c.username, c.password, tlsCfg)
	} else {
		cl, err = routeros.DialContext(ctx, c.address, c.username, c.password)
	}
	if err != nil {
		return nil, fmt.Errorf("routeros dial %s: %w", c.address, err)
	}
	c.conn = cl
	return cl, nil
}

func (c *APIClient) dropConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// Close tears down any cached connection. Safe to call multiple times.
func (c *APIClient) Close() error {
	c.dropConn()
	return nil
}

// runWithReconnect executes fn with a healthy connection. On error the
// connection is closed so a subsequent call re-dials — this turns a
// dropped TCP session into a one-cycle blip rather than a permanent stall.
func (c *APIClient) runWithReconnect(ctx context.Context, fn func(*routeros.Client) (*routeros.Reply, error)) (*routeros.Reply, error) {
	cl, err := c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := fn(cl)
	if err != nil {
		c.dropConn()
		return nil, err
	}
	return reply, nil
}

// List fetches every /ip/dns/static row and decodes it into a []Record.
// Empty comment fields round-trip as Record.Comment="" — the orchestrator
// uses that as the signal "not managed by an operator".
func (c *APIClient) List(ctx context.Context) ([]Record, error) {
	reply, err := c.runWithReconnect(ctx, func(cl *routeros.Client) (*routeros.Reply, error) {
		return cl.RunArgsContext(ctx, []string{"/ip/dns/static/print"})
	})
	if err != nil {
		return nil, fmt.Errorf("/ip/dns/static/print: %w", err)
	}
	out := make([]Record, 0, len(reply.Re))
	for _, sen := range reply.Re {
		rec, ok := sentenceToRecord(sen.Map)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	// Stable ordering for the orchestrator (and for test determinism).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// Add appends a /ip/dns/static row matching r and returns the assigned .id.
// Caller must populate r.Name, r.Type, the matching type-specific field,
// r.TTL (already as a RouterOS duration), and r.Comment (ownership label).
func (c *APIClient) Add(ctx context.Context, r Record) (string, error) {
	args, err := recordToAddArgs(r)
	if err != nil {
		return "", err
	}
	reply, err := c.runWithReconnect(ctx, func(cl *routeros.Client) (*routeros.Reply, error) {
		return cl.RunArgsContext(ctx, args)
	})
	if err != nil {
		return "", fmt.Errorf("/ip/dns/static/add %s/%s: %w", r.Type, r.Name, err)
	}
	if reply.Done == nil {
		return "", errors.New("routeros add: missing !done sentence")
	}
	return reply.Done.Map["ret"], nil
}

// Set updates an existing row by .id. Only type-specific rdata and ttl are
// mutated — name/type/comment must NOT be changed via Set (the orchestrator
// treats a same-name/same-type pair with a different owner as a collision
// and refuses to touch it long before reaching here).
func (c *APIClient) Set(ctx context.Context, id string, r Record) error {
	args, err := recordToSetArgs(id, r)
	if err != nil {
		return err
	}
	_, err = c.runWithReconnect(ctx, func(cl *routeros.Client) (*routeros.Reply, error) {
		return cl.RunArgsContext(ctx, args)
	})
	if err != nil {
		return fmt.Errorf("/ip/dns/static/set %s: %w", id, err)
	}
	return nil
}

// Remove deletes a row by .id.
func (c *APIClient) Remove(ctx context.Context, id string) error {
	_, err := c.runWithReconnect(ctx, func(cl *routeros.Client) (*routeros.Reply, error) {
		return cl.RunArgsContext(ctx, []string{"/ip/dns/static/remove", "=.id=" + id})
	})
	if err != nil {
		return fmt.Errorf("/ip/dns/static/remove %s: %w", id, err)
	}
	return nil
}

// Ping issues a cheap /system/identity/print to verify the API session is
// alive. Used by /healthz.
func (c *APIClient) Ping(ctx context.Context) error {
	_, err := c.runWithReconnect(ctx, func(cl *routeros.Client) (*routeros.Reply, error) {
		return cl.RunArgsContext(ctx, []string{"/system/identity/print"})
	})
	if err != nil {
		return fmt.Errorf("/system/identity/print: %w", err)
	}
	return nil
}

// --- decode helpers -------------------------------------------------------

// sentenceToRecord pulls a Record out of the proto.Sentence.Map RouterOS
// gives us. Returns (_, false) for rows the sidecar doesn't recognise so the
// caller can skip them safely. This includes:
//   - FWD/None and other unsupported rdata types;
//   - arbitrary user-authored `regexp` rows that don't match the fixed shape
//     this sidecar writes, OR that lack our ownership `comment`.
//
// For our own wildcard rows (regexp matches WildcardToRegexp's shape AND the
// row carries an ownership comment) the regexp is reverse-mapped back to the
// "*.…" wildcard FQDN and stored in Record.Name, so ListRecords surfaces the
// record and the operator does not see it as missing every tick.
func sentenceToRecord(m map[string]string) (Record, bool) {
	r := Record{
		ID:      m[".id"],
		Name:    m["name"],
		Regexp:  m["regexp"],
		Type:    strings.ToUpper(m["type"]),
		TTL:     m["ttl"],
		Comment: m["comment"],
		Address: m["address"],
		CNAME:   m["cname"],
		NS:      m["ns"],
		MXHost:  m["mx-exchange"],
	}
	if r.Name == "" && r.Regexp != "" {
		// Wildcard candidate. Only ours: must match our regexp shape AND be
		// owned (non-empty comment). Anything else is a user-authored row we
		// must never claim, modify, or surface.
		wildcard, ok := RegexpToWildcard(r.Regexp)
		if !ok || r.Comment == "" {
			return Record{}, false
		}
		r.Name = wildcard
	}
	if r.Type == "" {
		// RouterOS omits =type= on plain A rows in some builds. Mirror that
		// by inferring from which rdata field is set.
		switch {
		case r.Address != "" && strings.Contains(r.Address, ":"):
			r.Type = "AAAA"
		case r.Address != "":
			r.Type = "A"
		case r.CNAME != "":
			r.Type = "CNAME"
		case r.NS != "":
			r.Type = "NS"
		case r.MXHost != "":
			r.Type = "MX"
		}
	}
	if v, ok := m["mx-preference"]; ok && v != "" {
		// RouterOS sends mx-preference as an integer-formatted string.
		// Best-effort parse; on failure we leave MXPref=0 which the
		// orchestrator will surface as a malformed-MX warning.
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		r.MXPref = n
	}
	switch r.Type {
	case "A", "AAAA", "CNAME", "MX", "NS":
		// supported
	default:
		return Record{}, false
	}
	return r, true
}

// recordToAddArgs builds the RouterOS sentence for /ip/dns/static/add.
// A wildcard row is keyed by =regexp= (RouterOS' subdomain-matching field);
// every other row is keyed by =name= exactly as before. Exactly one of the
// two must be set.
func recordToAddArgs(r Record) ([]string, error) {
	if r.Name == "" && r.Regexp == "" {
		return nil, errors.New("Record.Name or Record.Regexp is required")
	}
	args := []string{"/ip/dns/static/add"}
	if r.Regexp != "" {
		args = append(args, "=regexp="+r.Regexp)
	} else {
		args = append(args, "=name="+r.Name)
	}
	args = append(args, "=type="+r.Type)
	if r.TTL != "" {
		args = append(args, "=ttl="+r.TTL)
	}
	if r.Comment != "" {
		args = append(args, "=comment="+r.Comment)
	}
	rd, err := rdataArgs(r)
	if err != nil {
		return nil, err
	}
	return append(args, rd...), nil
}

// recordToSetArgs builds the RouterOS sentence for /ip/dns/static/set.
// Comment is intentionally NOT included — once a row exists, ownership
// must never silently transfer between operators.
func recordToSetArgs(id string, r Record) ([]string, error) {
	if id == "" {
		return nil, errors.New("set: id is required")
	}
	args := []string{
		"/ip/dns/static/set",
		"=.id=" + id,
	}
	if r.TTL != "" {
		args = append(args, "=ttl="+r.TTL)
	}
	rd, err := rdataArgs(r)
	if err != nil {
		return nil, err
	}
	return append(args, rd...), nil
}

func rdataArgs(r Record) ([]string, error) {
	switch r.Type {
	case "A", "AAAA":
		if r.Address == "" {
			return nil, fmt.Errorf("%s: address is required", r.Type)
		}
		return []string{"=address=" + r.Address}, nil
	case "CNAME":
		if r.CNAME == "" {
			return nil, errors.New("CNAME: cname target is required")
		}
		return []string{"=cname=" + r.CNAME}, nil
	case "NS":
		if r.NS == "" {
			return nil, errors.New("NS: ns target is required")
		}
		return []string{"=ns=" + r.NS}, nil
	case "MX":
		if r.MXHost == "" {
			return nil, errors.New("MX: mx-exchange is required")
		}
		if r.MXPref < 0 || r.MXPref > 65535 {
			return nil, fmt.Errorf("MX: mx-preference out of range: %d", r.MXPref)
		}
		return []string{
			"=mx-exchange=" + r.MXHost,
			fmt.Sprintf("=mx-preference=%d", r.MXPref),
		}, nil
	}
	return nil, fmt.Errorf("unsupported type %q", r.Type)
}
