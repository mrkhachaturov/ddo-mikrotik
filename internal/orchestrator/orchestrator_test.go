package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mrkhachaturov/ddo-mikrotik/internal/mikrotik"
)

// fakeClient is a deterministic in-memory mikrotik.Client for unit tests.
// IDs are assigned monotonically so tests can assert against stable values.
type fakeClient struct {
	mu      sync.Mutex
	rows    []mikrotik.Record
	nextID  int
	addErr  error
	setErr  error
	rmErr   error
	listErr error
	pingErr error

	addCalls    []mikrotik.Record
	setCalls    []setCall
	removeCalls []string
	listCalls   int
}

type setCall struct {
	id     string
	record mikrotik.Record
}

func newFakeClient(initial ...mikrotik.Record) *fakeClient {
	f := &fakeClient{rows: append([]mikrotik.Record(nil), initial...)}
	for i := range f.rows {
		if f.rows[i].ID == "" {
			f.nextID++
			f.rows[i].ID = "*" + itoaHex(f.nextID)
		}
	}
	return f
}

func itoaHex(n int) string {
	const hexDigits = "0123456789ABCDEF"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{hexDigits[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

func (f *fakeClient) List(_ context.Context) ([]mikrotik.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := append([]mikrotik.Record(nil), f.rows...)
	return out, nil
}

func (f *fakeClient) Add(_ context.Context, r mikrotik.Record) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.nextID++
	r.ID = "*" + itoaHex(f.nextID)
	f.rows = append(f.rows, r)
	f.addCalls = append(f.addCalls, r)
	return r.ID, nil
}

func (f *fakeClient) Set(_ context.Context, id string, r mikrotik.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			r.ID = id
			r.Comment = f.rows[i].Comment // never mutated via /set
			f.rows[i] = r
			f.setCalls = append(f.setCalls, setCall{id, r})
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeClient) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rmErr != nil {
		return f.rmErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			f.removeCalls = append(f.removeCalls, id)
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeClient) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeClient) Close() error { return nil }

func defaultOpts() Options {
	return Options{Zones: []string{"home.lan"}, DefaultTTL: 3600}
}

// -- ListRecords -----------------------------------------------------------

// The sidecar surfaces every row whose `comment` is non-empty, regardless
// of which owner the comment carries. Labels["owner"] on each Endpoint
// reflects the value persisted in the row verbatim, so the operator can
// filter client-side. Unmanaged rows (empty comment) are excluded — the
// sidecar can't vouch for them.
func TestListRecords_RoundTripsOwnerFromComment(t *testing.T) {
	fc := newFakeClient(
		mikrotik.Record{Name: "app.home.lan", Type: "A", Address: "10.0.0.1", TTL: "1h", Comment: "docker-dns-operator:1"},
		mikrotik.Record{Name: "manual.home.lan", Type: "A", Address: "10.0.0.99", TTL: "1h", Comment: ""},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one managed endpoint, got %d", len(got))
	}
	if got[0].DNSName != "app.home.lan" || got[0].Labels["owner"] != "docker-dns-operator:1" {
		t.Fatalf("owner not round-tripped: %+v", got[0])
	}
	if got[0].RecordTTL != 3600 {
		t.Fatalf("TTL not decoded: %d", got[0].RecordTTL)
	}
}

// Mirrors rfc2136's TestListRecords_RoundTripsOwnerFromTxt: two operators
// with different ownership labels both round-trip through the same sidecar.
// The sidecar does NOT filter by owner — that's the operator's job.
func TestListRecords_TwoOperatorsBothRoundTrip(t *testing.T) {
	fc := newFakeClient(
		mikrotik.Record{Name: "a.home.lan", Type: "A", Address: "10.0.0.1", TTL: "1h", Comment: "docker-dns-operator:alpha"},
		mikrotik.Record{Name: "b.home.lan", Type: "A", Address: "10.0.0.2", TTL: "1h", Comment: "docker-dns-operator:beta"},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(got))
	}
	byName := map[string]string{}
	for _, e := range got {
		byName[e.DNSName] = e.Labels["owner"]
	}
	if byName["a.home.lan"] != "docker-dns-operator:alpha" || byName["b.home.lan"] != "docker-dns-operator:beta" {
		t.Fatalf("owners not preserved per row: %+v", byName)
	}
}

func TestListRecords_FilteredByConfiguredZones(t *testing.T) {
	fc := newFakeClient(
		mikrotik.Record{Name: "in.home.lan", Type: "A", Address: "10.0.0.1", Comment: "x"},
		mikrotik.Record{Name: "out.example.com", Type: "A", Address: "10.0.0.2", Comment: "x"},
	)
	o := New(Options{Zones: []string{"home.lan"}, DefaultTTL: 3600}, fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DNSName != "in.home.lan" {
		t.Fatalf("zone filter not applied: %+v", got)
	}
}

func TestListRecords_GroupsMultipleTargetsAndMxFormatsWithPriority(t *testing.T) {
	fc := newFakeClient(
		mikrotik.Record{Name: "mail.home.lan", Type: "MX", MXHost: "mx1.home.lan", MXPref: 10, Comment: "docker-dns-operator:1"},
		mikrotik.Record{Name: "mail.home.lan", Type: "MX", MXHost: "mx2.home.lan", MXPref: 20, Comment: "docker-dns-operator:1"},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Targets) != 2 {
		t.Fatalf("expected one MX endpoint with two targets, got %+v", got)
	}
	want := map[string]bool{"10 mx1.home.lan": true, "20 mx2.home.lan": true}
	for _, tgt := range got[0].Targets {
		if !want[tgt] {
			t.Fatalf("unexpected MX target %q", tgt)
		}
	}
}

// -- ApplyChanges create ---------------------------------------------------

// The most important contract test for the sidecar: a payload owner of
// "docker-dns-operator:alpha" must land on the row as `comment=...:alpha`
// verbatim. The sidecar must NEVER substitute its own value.
func TestApplyChanges_CreateHonorsOwnerFromPayload(t *testing.T) {
	fc := newFakeClient()
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName:    "app.home.lan",
			RecordType: "A",
			RecordTTL:  120,
			Targets:    []string{"10.1.2.3"},
			Labels:     map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 1 {
		t.Fatalf("expected one Add call, got %d", len(fc.addCalls))
	}
	added := fc.addCalls[0]
	if added.Comment != "docker-dns-operator:alpha" {
		t.Fatalf("comment must be the payload owner, got %q", added.Comment)
	}
	if added.Type != "A" || added.Address != "10.1.2.3" || added.Name != "app.home.lan" {
		t.Fatalf("rdata wrong: %+v", added)
	}
	if added.TTL != "2m" {
		t.Fatalf("TTL not converted to RouterOS duration: %q", added.TTL)
	}
}

func TestApplyChanges_CreateSkippedWhenOwnerLabelMissing(t *testing.T) {
	fc := newFakeClient()
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName:    "app.home.lan",
			RecordType: "A",
			Targets:    []string{"10.1.2.3"},
			// no Labels
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("must not add when owner missing, but added: %+v", fc.addCalls)
	}
}

func TestApplyChanges_CreateSkipsCollisionWithDifferentOwner(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.99", Comment: "docker-dns-operator:beta",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"10.1.2.3"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("collision must not add: %+v", fc.addCalls)
	}
}

func TestApplyChanges_DefaultTTLUsedWhenPayloadHasZero(t *testing.T) {
	fc := newFakeClient()
	o := New(Options{Zones: []string{"home.lan"}, DefaultTTL: 7200}, fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"1.2.3.4"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fc.addCalls[0].TTL != "2h" {
		t.Fatalf("default TTL not applied: %q", fc.addCalls[0].TTL)
	}
}

func TestApplyChanges_CreateMxParsesPriority(t *testing.T) {
	fc := newFakeClient()
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "mail.home.lan", RecordType: "MX", Targets: []string{"10 mx1.home.lan."},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 1 {
		t.Fatalf("expected one Add: %+v", fc.addCalls)
	}
	if fc.addCalls[0].MXPref != 10 || fc.addCalls[0].MXHost != "mx1.home.lan" {
		t.Fatalf("MX not parsed: %+v", fc.addCalls[0])
	}
}

// -- ApplyChanges update ---------------------------------------------------

func TestApplyChanges_UpdateSetsExistingRowSameOwner(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.1", TTL: "1h",
		Comment: "docker-dns-operator:alpha",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		UpdateOld: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
		UpdateNew: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"10.9.9.9"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.setCalls) != 1 {
		t.Fatalf("expected one Set call, got %d", len(fc.setCalls))
	}
	if fc.setCalls[0].record.Address != "10.9.9.9" {
		t.Fatalf("address not updated: %+v", fc.setCalls[0])
	}
}

func TestApplyChanges_UpdateSkippedOnOwnerMismatch(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.1",
		Comment: "docker-dns-operator:beta",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		UpdateOld: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
		UpdateNew: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"10.9.9.9"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.setCalls) != 0 {
		t.Fatalf("must not modify a row owned by a different operator")
	}
}

// -- ApplyChanges delete ---------------------------------------------------

func TestApplyChanges_DeleteRemovesOwnedRow(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.1",
		Comment: "docker-dns-operator:alpha",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A",
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 1 {
		t.Fatalf("expected one Remove call, got %d", len(fc.removeCalls))
	}
}

func TestApplyChanges_DeleteSkippedOnOwnerMismatch(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.1",
		Comment: "docker-dns-operator:beta",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A",
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 0 {
		t.Fatalf("must not delete a row owned by a different operator")
	}
}

func TestApplyChanges_DeleteSkippedWhenOwnerMissing(t *testing.T) {
	fc := newFakeClient(mikrotik.Record{
		Name: "app.home.lan", Type: "A", Address: "10.0.0.1",
		Comment: "docker-dns-operator:alpha",
	})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{DNSName: "app.home.lan", RecordType: "A"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 0 {
		t.Fatalf("must not delete when owner label missing")
	}
}

// -- ApplyChanges error propagation ---------------------------------------

func TestApplyChanges_ListErrorBubbles(t *testing.T) {
	fc := newFakeClient()
	fc.listErr = errors.New("dial timeout")
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{})
	if err == nil || !strings.Contains(err.Error(), "dial timeout") {
		t.Fatalf("expected list error to surface, got %v", err)
	}
}

func TestApplyChanges_AddErrorReturned(t *testing.T) {
	fc := newFakeClient()
	fc.addErr = errors.New("entry already exists")
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.home.lan", RecordType: "A", Targets: []string{"1.1.1.1"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "entry already exists") {
		t.Fatalf("expected add error, got %v", err)
	}
}
