package label

import "testing"

func TestTierOf(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   Tier
	}{
		{nil, ""},
		{[]string{Task, string(Working)}, ""},
		{[]string{Task, string(Light)}, Light},
		{[]string{string(Light), Task, string(Heavy)}, Heavy},
		{[]string{string(Normal), string(Light)}, Normal},
	} {
		if got := TierOf(c.labels); got != c.want {
			t.Errorf("TierOf(%q) = %q, want %q", c.labels, got, c.want)
		}
	}
}

func TestTierNamed(t *testing.T) {
	for name, want := range map[string]Tier{"light": Light, "normal": Normal, "heavy": Heavy} {
		if got, ok := TierNamed(name); !ok || got != want {
			t.Errorf("TierNamed(%q) = %q, %v, want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "tier:light", "Light", "extreme"} {
		if got, ok := TierNamed(name); ok {
			t.Errorf("TierNamed(%q) = %q, want none", name, got)
		}
	}
}
