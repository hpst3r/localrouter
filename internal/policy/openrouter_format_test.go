package policy

import "testing"

// usd must preserve the sign of a non-zero balance below one cent: a tiny
// overdraft is never "$0.00". The sub-cent threshold and exact strings match
// the dashboard's signedUSD formatter (0 < |f| < 0.01 -> "<$0.01"/"-<$0.01").
func TestUSDSubcentSign(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0.00"},
		{0.009, "<$0.01"},
		{-0.009, "-<$0.01"},
		{0.001, "<$0.01"},
		{-0.001, "-<$0.01"},
		{0.01, "$0.01"},
		{-0.01, "-$0.01"},
		{-0.076117902, "-$0.08"},
		{770.8176 - 770.893717902, "-$0.08"},
		{-1, "-$1.00"},
		{5, "$5.00"},
		{-5, "-$5.00"},
	}
	for _, c := range cases {
		if got := usd(c.in); got != c.want {
			t.Errorf("usd(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
