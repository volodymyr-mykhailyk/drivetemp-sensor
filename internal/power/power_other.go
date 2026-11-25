//go:build !linux

package power

// Check is not supported outside Linux.
func Check(string) (State, error) { return StateUnknown, ErrUnsupported }
