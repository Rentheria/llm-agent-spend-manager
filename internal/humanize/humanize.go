// Package humanize formats numbers for people. It exists so the same figure
// reads identically wherever it appears — CLI table, advice prose, quota
// report — instead of each layer growing its own separator logic.
package humanize

import "fmt"

// Int formats an integer with comma separators
// (e.g. 10107657766 -> "10,107,657,766").
func Int(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := ""
	if len(s) > 0 && s[0] == '-' {
		neg, s = "-", s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return neg + string(out)
}

// millionsThreshold is where digit-by-digit precision stops helping. Below it a
// reader compares exact numbers; above it they compare magnitudes, and nine
// digits of token count are harder to read than "780.6M".
const millionsThreshold = 1_000_000

// Millions abbreviates a large count in millions, falling back to plain
// separators below the threshold (e.g. 780628431 -> "780.6M", 4597 -> "4,597").
func Millions(v float64) string {
	if v > -millionsThreshold && v < millionsThreshold {
		return Int(int(v))
	}
	return fmt.Sprintf("%.1fM", v/millionsThreshold)
}
