package label

import "testing"

func TestTierOf(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   Tier
	}{
		{nil, Normal},
		{[]string{PRD, string(Working)}, Normal},
		{[]string{PRD, string(Light)}, Light},
		{[]string{string(Light), PRD, string(Heavy)}, Heavy},
		{[]string{string(Normal), string(Light)}, Normal},
	} {
		if got := TierOf(c.labels); got != c.want {
			t.Errorf("TierOf(%q) = %q, want %q", c.labels, got, c.want)
		}
	}
}
