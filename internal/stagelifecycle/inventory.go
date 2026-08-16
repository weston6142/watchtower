package stagelifecycle

// DurableBoundaries returns lifecycle checkpoints in production transition
// order as a fresh slice.
func DurableBoundaries() []Substate {
	boundaries := make([]Substate, 0, 6)
	for current := Substate(""); ; {
		next, ok := Next(current)
		if !ok {
			return boundaries
		}
		boundaries = append(boundaries, next)
		current = next
	}
}
