package mikrotik

import "testing"

func TestSecondsToDuration(t *testing.T) {
	cases := []struct {
		secs int
		want string
	}{
		{0, "0s"},
		{1, "1s"},
		{60, "1m"},
		{61, "1m1s"},
		{3600, "1h"},
		{3661, "1h1m1s"},
		{86400, "1d"},
		{90061, "1d1h1m1s"},
	}
	for _, c := range cases {
		if got := SecondsToDuration(c.secs); got != c.want {
			t.Errorf("SecondsToDuration(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestDurationToSeconds(t *testing.T) {
	cases := []struct {
		s    string
		want int
		fail bool
	}{
		{"", 0, false},
		{"45s", 45, false},
		{"1m", 60, false},
		{"1h", 3600, false},
		{"1h30m", 5400, false},
		{"2d4h", 187200, false},
		{"1h 30m", 5400, false}, // whitespace tolerated
		{"5x", 0, true},
		{"15", 0, true}, // trailing digits without a unit
	}
	for _, c := range cases {
		got, err := DurationToSeconds(c.s)
		if c.fail {
			if err == nil {
				t.Errorf("DurationToSeconds(%q) expected error, got %d", c.s, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DurationToSeconds(%q) unexpected error: %v", c.s, err)
			continue
		}
		if got != c.want {
			t.Errorf("DurationToSeconds(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	for _, secs := range []int{0, 1, 59, 60, 3599, 3600, 3661, 86400, 90061, 123456} {
		s := SecondsToDuration(secs)
		got, err := DurationToSeconds(s)
		if err != nil {
			t.Fatalf("round-trip %d -> %q -> err: %v", secs, s, err)
		}
		if got != secs {
			t.Errorf("round-trip %d -> %q -> %d", secs, s, got)
		}
	}
}
