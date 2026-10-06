package label

import "slices"

type Tier string

const (
	Light  Tier = "tier:light"
	Normal Tier = "tier:normal"
	Heavy  Tier = "tier:heavy"
)

// Heaviest first: TierOf takes the first one a PRD has, so the heaviest of several labels wins.
var tiers = []Tier{Heavy, Normal, Light}

// TierOf returns the heaviest tier in labels, or "" when there is none.
func TierOf(labels []string) Tier {
	for _, t := range tiers {
		if slices.Contains(labels, string(t)) {
			return t
		}
	}
	return ""
}
