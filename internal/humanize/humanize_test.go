package humanize

import (
	"testing"
	"time"
)

func TestInt_GroupsByThousands(t *testing.T) {
	cases := map[int]string{
		0:           "0",
		7:           "7",
		999:         "999",
		1000:        "1,000",
		10107657766: "10,107,657,766",
		-4500:       "-4,500",
	}
	for in, want := range cases {
		if got := Int(in); got != want {
			t.Errorf("Int(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestMillions_AbbreviatesOnlyAboveTheThreshold(t *testing.T) {
	cases := map[float64]string{
		4597:       "4,597",
		999_999:    "999,999",
		1_000_000:  "1.0M",
		780628431:  "780.6M",
		-2_500_000: "-2.5M",
	}
	for in, want := range cases {
		if got := Millions(in); got != want {
			t.Errorf("Millions(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration_ReadsTheWayAWaitIsSpoken(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Hour:                   "0 min",
		0:                            "0 min",
		45 * time.Minute:             "45 min",
		time.Hour:                    "1 h 0 min",
		3*time.Hour + 28*time.Minute: "3 h 28 min",
		30 * time.Hour:               "1 d 6 h",
	}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%s) = %q, want %q", in, got, want)
		}
	}
}
