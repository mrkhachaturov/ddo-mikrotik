package mikrotik

import "strings"

// RouterOS static DNS has two ways to name a row: a literal `name` (an exact
// FQDN) and a `regexp` (a POSIX ERE matched against the query name). A DNS
// wildcard like "*.dev.example.com" must NOT be stored as `name=*.dev…` —
// RouterOS treats the asterisk as a literal character, so the row matches
// nothing. Wildcards must go through `regexp`.
//
// We use one fixed, reversible regexp shape so we can recognise our own rows
// on read and reverse-map them back to the wildcard FQDN. The shape is:
//
//	^.*\.<escaped-base>$
//
// where <base> is the wildcard's suffix (everything after the leading "*.")
// with each "." escaped to "\.". The leading ".*\." requires at least one
// label before the base, so the apex (the base itself) does NOT match — this
// keeps the wildcard and a possible apex record as distinct rows, preserving
// the 1:1 record/ownership mapping. Anchoring at both ends (^ … $) makes the
// shape unambiguous to reverse and avoids accidentally claiming an arbitrary
// user-authored regexp as ours.

const (
	wildcardPrefix = "*."    // DNS wildcard label prefix
	regexpHead     = `^.*\.` // our regexp shape: anchor + any-leading-label + dot
	regexpTail     = `$`     // our regexp shape: end anchor
)

// WildcardToRegexp converts a DNS wildcard name ("*.dev.example.com") into the
// RouterOS regexp we store. Returns ("", false) for any name that is not a
// "*." wildcard or that has an empty base.
func WildcardToRegexp(name string) (string, bool) {
	if !strings.HasPrefix(name, wildcardPrefix) {
		return "", false
	}
	base := strings.TrimPrefix(name, wildcardPrefix)
	if base == "" {
		return "", false
	}
	return regexpHead + escapeDots(base) + regexpTail, true
}

// RegexpToWildcard reverses WildcardToRegexp. It only recognises the exact
// shape WE write (^.*\.<escaped-base>$); any other regexp — a user-authored
// pattern — returns ("", false) so it is left untouched.
func RegexpToWildcard(rx string) (string, bool) {
	if !strings.HasPrefix(rx, regexpHead) || !strings.HasSuffix(rx, regexpTail) {
		return "", false
	}
	escaped := strings.TrimSuffix(strings.TrimPrefix(rx, regexpHead), regexpTail)
	if escaped == "" {
		return "", false
	}
	base := unescapeDots(escaped)
	// A faithful round-trip: the unescaped base, re-escaped, must reproduce
	// the original — this rejects regexps that merely share our prefix/suffix
	// but contain other ERE metacharacters in the middle.
	if regexpHead+escapeDots(base)+regexpTail != rx {
		return "", false
	}
	return wildcardPrefix + base, true
}

// escapeDots escapes every "." as "\." (the only ERE metacharacter that
// appears in a DNS label sequence). Hostnames are otherwise [a-z0-9-].
func escapeDots(s string) string {
	return strings.ReplaceAll(s, ".", `\.`)
}

func unescapeDots(s string) string {
	return strings.ReplaceAll(s, `\.`, ".")
}
