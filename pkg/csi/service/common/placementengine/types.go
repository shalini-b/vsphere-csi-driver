package placementengine

// PlacementFinalError is raised when volume provisioning
// cannot be completed on a particular VC even when retried.
type PlacementFinalError struct {
	ErrMsg string
}

func (e *PlacementFinalError) Error() string {
	return e.ErrMsg
}
