package mikrotik

import "testing"

// The write form: a DNS wildcard "*.dev.example.com" maps to a RouterOS
// regexp that matches a leading label plus the escaped base, anchored at
// both ends so the apex (dev.example.com) does NOT match.
func TestWildcardToRegexp(t *testing.T) {
	cases := []struct {
		name string // wildcard DNS name
		want string // RouterOS regexp
		ok   bool
	}{
		{"*.dev.example.com", `^.*\.dev\.example\.com$`, true},
		{"*.home.lan", `^.*\.home\.lan$`, true},
		{"app.home.lan", "", false}, // not a wildcard
		{"*", "", false},            // bare star, no base
		{"*.", "", false},           // star with empty base
	}
	for _, c := range cases {
		got, ok := WildcardToRegexp(c.name)
		if ok != c.ok {
			t.Errorf("WildcardToRegexp(%q) ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if got != c.want {
			t.Errorf("WildcardToRegexp(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// The read reverse-map: only regexp values that match the exact shape WE
// write reverse-map back to a wildcard DNS name. Arbitrary user regexps do
// not.
func TestRegexpToWildcard(t *testing.T) {
	cases := []struct {
		regexp string
		want   string
		ok     bool
	}{
		{`^.*\.dev\.example\.com$`, "*.dev.example.com", true},
		{`^.*\.home\.lan$`, "*.home.lan", true},
		{`.*\.dev\.example\.com`, "", false}, // missing our anchors
		{`^foo\.example\.com$`, "", false},   // user-authored literal-ish regexp
		{`^.*$`, "", false},                  // no base
		{"", "", false},                      // empty
	}
	for _, c := range cases {
		got, ok := RegexpToWildcard(c.regexp)
		if ok != c.ok {
			t.Errorf("RegexpToWildcard(%q) ok = %v, want %v", c.regexp, ok, c.ok)
			continue
		}
		if got != c.want {
			t.Errorf("RegexpToWildcard(%q) = %q, want %q", c.regexp, got, c.want)
		}
	}
}

func TestWildcardRoundTrip(t *testing.T) {
	for _, name := range []string{"*.dev.example.com", "*.home.lan", "*.a.b.c.d"} {
		rx, ok := WildcardToRegexp(name)
		if !ok {
			t.Fatalf("WildcardToRegexp(%q) failed", name)
		}
		back, ok := RegexpToWildcard(rx)
		if !ok {
			t.Fatalf("RegexpToWildcard(%q) failed", rx)
		}
		if back != name {
			t.Errorf("round-trip %q -> %q -> %q", name, rx, back)
		}
	}
}

// recordToAddArgs must emit =regexp= (and NOT =name=) when Regexp is set,
// and continue to emit =name= for ordinary records.
func TestRecordToAddArgs_RegexpRow(t *testing.T) {
	r := Record{
		Regexp:  `^.*\.dev\.example\.com$`,
		Type:    "A",
		Address: "10.0.0.1",
		TTL:     "1h",
		Comment: "docker-dns-operator:alpha",
	}
	args, err := recordToAddArgs(r)
	if err != nil {
		t.Fatalf("recordToAddArgs: %v", err)
	}
	joined := args
	hasRegexp := false
	for _, a := range joined {
		if a == "=regexp="+r.Regexp {
			hasRegexp = true
		}
		if len(a) >= 6 && a[:6] == "=name=" {
			t.Fatalf("regexp row must not emit =name=: %q", a)
		}
	}
	if !hasRegexp {
		t.Fatalf("expected =regexp= arg, got %v", joined)
	}
}

func TestRecordToAddArgs_RegexpRowRequiresType(t *testing.T) {
	if _, err := recordToAddArgs(Record{}); err == nil {
		t.Fatal("expected error when neither Name nor Regexp is set")
	}
}

// sentenceToRecord must reverse-map our regexp rows (with our ownership
// comment) into a Record carrying the wildcard Name, and must SKIP a
// user-authored regexp row that lacks our comment.
func TestSentenceToRecord_OurRegexpRow(t *testing.T) {
	rec, ok := sentenceToRecord(map[string]string{
		".id":     "*5",
		"regexp":  `^.*\.dev\.example\.com$`,
		"type":    "A",
		"address": "10.0.0.1",
		"ttl":     "1h",
		"comment": "docker-dns-operator:alpha",
	})
	if !ok {
		t.Fatal("our regexp row should be recognised")
	}
	if rec.Name != "*.dev.example.com" {
		t.Fatalf("regexp not reverse-mapped to wildcard name: %q", rec.Name)
	}
	if rec.Regexp != `^.*\.dev\.example\.com$` {
		t.Fatalf("Regexp not preserved: %q", rec.Regexp)
	}
	if rec.Type != "A" || rec.Address != "10.0.0.1" {
		t.Fatalf("rdata wrong: %+v", rec)
	}
}

func TestSentenceToRecord_UserRegexpRowSkipped(t *testing.T) {
	// No ownership comment AND a regexp that doesn't match our shape.
	if _, ok := sentenceToRecord(map[string]string{
		".id":     "*7",
		"regexp":  `\.example\.com`,
		"type":    "A",
		"address": "10.0.0.1",
	}); ok {
		t.Fatal("user-authored regexp row without our comment must be skipped")
	}
	// Our exact shape but NO ownership comment: still skip on read (not ours).
	if _, ok := sentenceToRecord(map[string]string{
		".id":     "*8",
		"regexp":  `^.*\.dev\.example\.com$`,
		"type":    "A",
		"address": "10.0.0.1",
	}); ok {
		t.Fatal("regexp row without our ownership comment must be skipped")
	}
}
