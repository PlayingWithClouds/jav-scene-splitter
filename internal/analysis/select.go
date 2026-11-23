package analysis

// SelectOptions decide which confirmed transitions become scene boundaries.
type SelectOptions struct {
	// EdgeMargin drops transitions this close to the start or end (opening
	// and closing fades).
	EdgeMargin float64
	// MinScene drops a transition this close to an already kept one.
	MinScene float64
}

// transitionRank prefers fades and flashes over dissolves when two confirmed
// transitions compete for the same spot.
func transitionRank(transition string) int {
	if transition == "dissolve" {
		return 1
	}
	return 0
}

// SelectBoundaries keeps transitions away from the edges and at least
// MinScene apart, preferring fades over dissolves. Input and output are in
// time order.
func SelectBoundaries(boundaries []Boundary, duration float64, options SelectOptions) []Boundary {
	var kept []Boundary
	for _, boundary := range boundaries {
		if boundary.Time < options.EdgeMargin || duration-boundary.Time < options.EdgeMargin {
			continue
		}
		if len(kept) == 0 || boundary.Time-kept[len(kept)-1].Time >= options.MinScene {
			kept = append(kept, boundary)
			continue
		}
		last := &kept[len(kept)-1]
		if transitionRank(boundary.Transition) < transitionRank(last.Transition) {
			*last = boundary
		}
	}
	return kept
}
