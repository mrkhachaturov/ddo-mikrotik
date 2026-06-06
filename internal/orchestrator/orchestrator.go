// Package orchestrator owns the high-level MikroTik logic the operator
// used to drive over the wire: list managed records (every row whose
// `comment` is non-empty), apply external-dns Changes (create/update/delete
// /ip/dns/static rows), and honour the ownership-label round-trip without
// the sidecar inventing its own owner identity.
//
// The HTTP handler layer translates external-dns webhook v1 requests into
// calls on this package; the mikrotik client layer owns the RouterOS
// protocol details (binary API sentences, type-specific rdata fields).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/mrkhachaturov/ddo-mikrotik/internal/mikrotik"
)

// Options configures an Orchestrator. All fields are required (DefaultTTL
// may be zero in tests; the production config layer sets a positive value).
//
// Note: there is no ownership-label field here. The sidecar is
// intentionally ownership-agnostic — it round-trips whatever value the
// caller stamps on Labels["owner"] of each Endpoint through the persistent
// RouterOS `comment` field, and surfaces it back on ListRecords. The caller
// (operator) owns the identity concept; the sidecar is a dumb passthrough.
type Options struct {
	Zones      []string
	DefaultTTL int
}

// Endpoint is the wire-compatible external-dns endpoint shape. We carry
// only the fields the sidecar can faithfully round-trip; extras like
// ProviderSpecific are accepted on the way in but otherwise ignored.
type Endpoint struct {
	DNSName    string
	Targets    []string
	RecordType string
	RecordTTL  int64
	Labels     map[string]string
}

// Changes is the inbound change-set from external-dns POST /records.
type Changes struct {
	Create    []*Endpoint
	UpdateOld []*Endpoint
	UpdateNew []*Endpoint
	Delete    []*Endpoint
}

// Orchestrator is the runtime object. Construct once at boot.
type Orchestrator struct {
	opts   Options
	client mikrotik.Client

	// applyMu serialises ApplyChanges across concurrent webhook callers.
	// The RouterOS API is per-connection sequential; we play it safe and
	// serialise on the sidecar side too — volume is one call per CRON tick.
	applyMu sync.Mutex
}

// New constructs an Orchestrator over the given mikrotik client.
func New(opts Options, client mikrotik.Client) *Orchestrator {
	return &Orchestrator{opts: opts, client: client}
}

// Zones returns the configured zone list (copy) for the GET / handler.
func (o *Orchestrator) Zones() []string {
	out := make([]string, len(o.opts.Zones))
	copy(out, o.opts.Zones)
	return out
}

// ListRecords pulls every /ip/dns/static row, drops the rows that aren't
// operator-managed (empty `comment`), groups the rest by (name, type), and
// emits an Endpoint per group. Labels["owner"] is populated verbatim from
// the row's comment field — the operator-side WebhookProvider filters by
// owner downstream so two operators with different owner labels each see
// only their own records.
func (o *Orchestrator) ListRecords(ctx context.Context) ([]*Endpoint, error) {
	rows, err := o.client.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	type key struct{ name, rtype string }
	bucket := map[key]*Endpoint{}
	var order []key

	for _, r := range rows {
		if r.Comment == "" {
			// Not operator-managed — the sidecar can't vouch for it.
			continue
		}
		// recordName reverse-maps our wildcard regexp rows ("*.…") and returns
		// the literal name for normal rows. A regexp row that isn't one of ours
		// resolves to "" — skip it rather than surface a nameless endpoint.
		name := recordName(r)
		if name == "" {
			continue
		}
		if !o.matchesZoneFilter(name) {
			continue
		}
		k := key{name, strings.ToUpper(r.Type)}
		ep, ok := bucket[k]
		if !ok {
			ttlSecs := 0
			if r.TTL != "" {
				if v, err := mikrotik.DurationToSeconds(r.TTL); err == nil {
					ttlSecs = v
				}
			}
			ep = &Endpoint{
				DNSName:    k.name,
				RecordType: k.rtype,
				RecordTTL:  int64(ttlSecs),
				Labels:     map[string]string{"owner": r.Comment},
			}
			bucket[k] = ep
			order = append(order, k)
		}
		switch k.rtype {
		case "A", "AAAA":
			ep.Targets = append(ep.Targets, r.Address)
		case "CNAME":
			ep.Targets = append(ep.Targets, r.CNAME)
		case "NS":
			ep.Targets = append(ep.Targets, r.NS)
		case "MX":
			ep.Targets = append(ep.Targets, fmt.Sprintf("%d %s", r.MXPref, r.MXHost))
		}
	}

	// Deterministic ordering: by name asc, type asc.
	sort.Slice(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].rtype < order[j].rtype
	})
	out := make([]*Endpoint, 0, len(order))
	for _, k := range order {
		out = append(out, bucket[k])
	}
	return out, nil
}

// ApplyChanges fans out a webhook Changes payload to RouterOS row
// operations. The ownership rules:
//
//   - Create/Update/Delete require Labels["owner"] on the payload Endpoint.
//     Missing label => log warning and skip the entry (contract violation
//     by the caller; we never invent an owner ourselves).
//   - For Create: refuse if a same-(name, type) row already exists with a
//     different non-empty comment (different owner) — that's a collision.
//   - For Update/Delete: match the existing row by (name, type) AND
//     comment equal to the payload owner. A mismatched comment is a
//     collision: log and skip without touching the row.
//   - All writes set the row's comment to the payload owner verbatim.
//
// Returns the first error encountered, after attempting every entry. The
// rest of the entries still get a best-effort apply so a single bad entry
// doesn't strand the rest of the cycle.
func (o *Orchestrator) ApplyChanges(ctx context.Context, ch Changes) error {
	o.applyMu.Lock()
	defer o.applyMu.Unlock()

	// Snapshot existing rows so we can look up by (name, type) without
	// chatting to the router for every change.
	existing, err := o.client.List(ctx)
	if err != nil {
		return fmt.Errorf("list (apply): %w", err)
	}
	byKey := indexByNameType(existing)

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Process deletes first to free up name/type slots before creates.
	for _, e := range ch.Delete {
		record(o.applyDelete(ctx, e, byKey))
	}

	// Then paired updates.
	n := len(ch.UpdateOld)
	if n > len(ch.UpdateNew) {
		n = len(ch.UpdateNew)
	}
	for i := 0; i < n; i++ {
		record(o.applyUpdate(ctx, ch.UpdateOld[i], ch.UpdateNew[i], byKey))
	}

	// Finally creates.
	for _, e := range ch.Create {
		record(o.applyCreate(ctx, e, byKey))
	}

	return firstErr
}

// --- per-operation handlers ----------------------------------------------

func (o *Orchestrator) applyCreate(ctx context.Context, e *Endpoint, byKey map[recKey][]mikrotik.Record) error {
	if e == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(e)
	if !ok {
		log.Printf("orchestrator: skip create %s — Labels[\"owner\"] missing on payload", e.DNSName)
		return nil
	}
	if !o.matchesZoneFilter(e.DNSName) {
		log.Printf("orchestrator: skip create %s — outside configured zones", e.DNSName)
		return nil
	}
	recs, err := o.endpointToRecords(e, owner)
	if err != nil {
		log.Printf("orchestrator: skip create %s — %v", e.DNSName, err)
		return nil
	}
	for _, r := range recs {
		k := recKey{recordName(r), r.Type}
		// Collision: same name/type with a different non-empty comment is a
		// row belonging to a different operator. Refuse to clobber.
		clash := false
		for _, ex := range byKey[k] {
			if ex.Comment != "" && ex.Comment != owner {
				log.Printf("orchestrator: skip create %s/%s — existing row owned by %q (request owner %q)",
					r.Type, k.name, ex.Comment, owner)
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		// CNAME mutual exclusion with other types at the same name
		// (RFC 1034 §3.6.2). RouterOS itself enforces this on add but
		// we surface a clearer log line.
		if r.Type == "CNAME" && hasOtherTypeAtName(byKey, k.name, "CNAME") {
			log.Printf("orchestrator: skip create CNAME %s — other types already present at name", k.name)
			continue
		}
		if r.Type != "CNAME" && hasOtherTypeAtName(byKey, k.name, "") && hasTypeAtName(byKey, k.name, "CNAME") {
			log.Printf("orchestrator: skip create %s/%s — CNAME present at name", r.Type, k.name)
			continue
		}
		id, err := o.client.Add(ctx, r)
		if err != nil {
			log.Printf("orchestrator: add %s/%s failed: %v", r.Type, k.name, err)
			return err
		}
		// Update the local index so subsequent operations in the same
		// payload see the new row.
		stored := r
		stored.ID = id
		byKey[k] = append(byKey[k], stored)
	}
	return nil
}

func (o *Orchestrator) applyUpdate(ctx context.Context, oldE, newE *Endpoint, byKey map[recKey][]mikrotik.Record) error {
	if oldE == nil || newE == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(newE)
	if !ok {
		log.Printf("orchestrator: skip update %s — Labels[\"owner\"] missing on updateNew payload", newE.DNSName)
		return nil
	}
	if !o.matchesZoneFilter(newE.DNSName) {
		log.Printf("orchestrator: skip update %s — outside configured zones", newE.DNSName)
		return nil
	}
	newRecs, err := o.endpointToRecords(newE, owner)
	if err != nil {
		log.Printf("orchestrator: skip update %s — %v", newE.DNSName, err)
		return nil
	}
	if len(newRecs) == 0 {
		return nil
	}
	k := recKey{normalizeName(newE.DNSName), strings.ToUpper(newE.RecordType)}
	candidates := byKey[k]
	// Filter candidates by owner: only the rows whose comment matches the
	// payload owner are eligible. A mismatch is a collision — log and skip.
	owned := make([]mikrotik.Record, 0, len(candidates))
	for _, c := range candidates {
		if c.Comment == owner {
			owned = append(owned, c)
		} else if c.Comment != "" {
			log.Printf("orchestrator: skip update %s/%s — existing row owned by %q (request owner %q)",
				k.rtype, k.name, c.Comment, owner)
		}
	}
	if len(owned) == 0 {
		log.Printf("orchestrator: skip update %s/%s — no owned row to update", k.rtype, k.name)
		return nil
	}
	// Multi-target updates: align by index where possible, otherwise
	// remove leftover owned rows and add new ones.
	pair := len(owned)
	if len(newRecs) < pair {
		pair = len(newRecs)
	}
	for i := 0; i < pair; i++ {
		if err := o.client.Set(ctx, owned[i].ID, newRecs[i]); err != nil {
			log.Printf("orchestrator: set %s/%s failed: %v", k.rtype, k.name, err)
			return err
		}
		owned[i] = mergeForIndex(owned[i], newRecs[i])
	}
	// Surplus existing rows: delete.
	for i := pair; i < len(owned); i++ {
		if err := o.client.Remove(ctx, owned[i].ID); err != nil {
			log.Printf("orchestrator: remove (update surplus) %s failed: %v", owned[i].ID, err)
			return err
		}
	}
	// Surplus new rows: add.
	for i := pair; i < len(newRecs); i++ {
		id, err := o.client.Add(ctx, newRecs[i])
		if err != nil {
			log.Printf("orchestrator: add (update surplus) %s/%s failed: %v", k.rtype, k.name, err)
			return err
		}
		added := newRecs[i]
		added.ID = id
		owned = append(owned, added)
	}
	byKey[k] = owned
	return nil
}

func (o *Orchestrator) applyDelete(ctx context.Context, e *Endpoint, byKey map[recKey][]mikrotik.Record) error {
	if e == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(e)
	if !ok {
		log.Printf("orchestrator: skip delete %s — Labels[\"owner\"] missing on payload", e.DNSName)
		return nil
	}
	if !o.matchesZoneFilter(e.DNSName) {
		log.Printf("orchestrator: skip delete %s — outside configured zones", e.DNSName)
		return nil
	}
	k := recKey{normalizeName(e.DNSName), strings.ToUpper(e.RecordType)}
	remaining := byKey[k][:0]
	for _, row := range byKey[k] {
		if row.Comment != owner {
			if row.Comment != "" {
				log.Printf("orchestrator: skip delete %s/%s — existing row owned by %q (request owner %q)",
					k.rtype, k.name, row.Comment, owner)
			}
			remaining = append(remaining, row)
			continue
		}
		if err := o.client.Remove(ctx, row.ID); err != nil {
			log.Printf("orchestrator: remove %s failed: %v", row.ID, err)
			return err
		}
	}
	byKey[k] = remaining
	return nil
}

// --- helpers --------------------------------------------------------------

type recKey struct{ name, rtype string }

// recordName returns the logical DNS name used for indexing/keying a row.
// Rows read from the router already have their wildcard regexp reverse-mapped
// into Name by sentenceToRecord, so Name is authoritative there. Rows we
// build for Add carry the wildcard in Regexp with an empty Name, so we derive
// the same "*.…" name from Regexp here. This keeps the create-collision and
// update/delete lookups symmetric between built and read records.
func recordName(r mikrotik.Record) string {
	if r.Name == "" && r.Regexp != "" {
		if w, ok := mikrotik.RegexpToWildcard(r.Regexp); ok {
			return normalizeName(w)
		}
	}
	return normalizeName(r.Name)
}

func indexByNameType(rows []mikrotik.Record) map[recKey][]mikrotik.Record {
	out := map[recKey][]mikrotik.Record{}
	for _, r := range rows {
		k := recKey{recordName(r), strings.ToUpper(r.Type)}
		out[k] = append(out[k], r)
	}
	return out
}

func hasOtherTypeAtName(byKey map[recKey][]mikrotik.Record, name, excludeType string) bool {
	for k := range byKey {
		if k.name == name && k.rtype != excludeType && len(byKey[k]) > 0 {
			return true
		}
	}
	return false
}

func hasTypeAtName(byKey map[recKey][]mikrotik.Record, name, rtype string) bool {
	return len(byKey[recKey{name, rtype}]) > 0
}

func (o *Orchestrator) matchesZoneFilter(fqdn string) bool {
	if len(o.opts.Zones) == 0 {
		return true
	}
	name := normalizeName(fqdn)
	for _, z := range o.opts.Zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

func normalizeName(fqdn string) string {
	return strings.ToLower(strings.TrimSuffix(fqdn, "."))
}

// ownerFromEndpoint pulls Labels["owner"] off an Endpoint. Returns
// ("", false) when missing/empty — callers treat that as a contract
// violation by the caller (operator must always stamp it).
func ownerFromEndpoint(e *Endpoint) (string, bool) {
	if e == nil || e.Labels == nil {
		return "", false
	}
	v, ok := e.Labels["owner"]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// endpointToRecords expands a single multi-target Endpoint into one or
// more mikrotik.Record values, one per target. TTL falls back to the
// configured default when the payload omits recordTTL. Comment is set
// verbatim from the resolved owner (caller-supplied).
func (o *Orchestrator) endpointToRecords(e *Endpoint, owner string) ([]mikrotik.Record, error) {
	if e == nil || len(e.Targets) == 0 {
		return nil, nil
	}
	rtype := strings.ToUpper(e.RecordType)
	switch rtype {
	case "A", "AAAA", "CNAME", "NS", "MX":
	default:
		return nil, fmt.Errorf("unsupported recordType %q", rtype)
	}
	ttlSecs := int(e.RecordTTL)
	if ttlSecs <= 0 {
		ttlSecs = o.opts.DefaultTTL
	}
	ttl := ""
	if ttlSecs > 0 {
		ttl = mikrotik.SecondsToDuration(ttlSecs)
	}
	name := normalizeName(e.DNSName)
	// Wildcard names ("*.dev.example.com") cannot be stored as a literal
	// =name= (RouterOS would treat the asterisk as a dead literal). Translate
	// to the RouterOS =regexp= form instead, leaving Name empty. Exactly one
	// RouterOS row per endpoint either way — no match-subdomain on a literal
	// name, which would conflate the apex and break the 1:1 ownership mapping.
	regexp := ""
	if rx, ok := mikrotik.WildcardToRegexp(name); ok {
		regexp = rx
		name = ""
	}
	out := make([]mikrotik.Record, 0, len(e.Targets))
	for _, t := range e.Targets {
		r := mikrotik.Record{
			Name:    name,
			Regexp:  regexp,
			Type:    rtype,
			TTL:     ttl,
			Comment: owner,
		}
		v := strings.TrimSpace(t)
		if v == "" {
			continue
		}
		switch rtype {
		case "A", "AAAA":
			r.Address = strings.ToLower(v)
		case "CNAME":
			r.CNAME = strings.TrimSuffix(strings.ToLower(v), ".")
		case "NS":
			r.NS = strings.TrimSuffix(strings.ToLower(v), ".")
		case "MX":
			parts := strings.Fields(v)
			if len(parts) != 2 {
				return nil, fmt.Errorf("MX target %q: expected '<priority> <host>'", v)
			}
			pref, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("MX target %q: priority not numeric", v)
			}
			r.MXPref = pref
			r.MXHost = strings.TrimSuffix(strings.ToLower(parts[1]), ".")
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("no usable targets")
	}
	return out, nil
}

// mergeForIndex returns the existing row with its id intact and the new
// rdata applied, so the in-memory index stays consistent with what's now
// on the router.
func mergeForIndex(existing mikrotik.Record, applied mikrotik.Record) mikrotik.Record {
	out := applied
	out.ID = existing.ID
	out.Comment = existing.Comment // never mutated via /set
	return out
}
