//go:build !linux

package cli

// Leave the leader unreaped until readiness or the coordinator deadline on
// platforms where we don't have a non-reaping exit check.
func workerExited(int) (bool, error) { return false, nil }
