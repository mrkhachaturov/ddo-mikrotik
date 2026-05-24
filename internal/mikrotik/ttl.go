package mikrotik

import (
	"fmt"
	"strings"
	"unicode"
)

// SecondsToDuration converts an integer second count into the RouterOS
// duration string format ("1h", "30m1s", "1d2h"). Mirrors the operator's
// secondsToMikrotikTTL helper byte-for-byte for record-level continuity.
//
// Returns "0s" for zero (the orchestrator avoids that path; this just
// makes the function total).
func SecondsToDuration(total int) string {
	if total < 0 {
		total = 0
	}
	days := total / 86400
	rem := total % 86400
	hours := rem / 3600
	rem %= 3600
	mins := rem / 60
	secs := rem % 60

	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	if mins > 0 {
		fmt.Fprintf(&b, "%dm", mins)
	}
	if secs > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", secs)
	}
	return b.String()
}

// DurationToSeconds parses RouterOS' duration string ("1h30m", "45s",
// "2d4h") back into integer seconds. Unknown unit characters cause an
// error so a typo doesn't silently round-trip as zero.
func DurationToSeconds(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	total := 0
	cur := 0
	have := false
	for _, ch := range s {
		switch {
		case unicode.IsDigit(ch):
			cur = cur*10 + int(ch-'0')
			have = true
		case ch == 'd':
			total += cur * 86400
			cur, have = 0, false
		case ch == 'h':
			total += cur * 3600
			cur, have = 0, false
		case ch == 'm':
			total += cur * 60
			cur, have = 0, false
		case ch == 's':
			total += cur
			cur, have = 0, false
		case unicode.IsSpace(ch):
			// tolerate "1h 30m" form
		default:
			return 0, fmt.Errorf("invalid duration %q (bad unit %q)", s, ch)
		}
	}
	if have {
		return 0, fmt.Errorf("invalid duration %q (trailing digits)", s)
	}
	return total, nil
}
