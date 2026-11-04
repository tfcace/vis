package vis

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

import "math"

func sqrt(x float64) float64 { return math.Sqrt(x) }
func exp(x float64) float64  { return math.Exp(x) }
