package humanize

import "testing"

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
