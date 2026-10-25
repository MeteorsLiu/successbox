//go:build !linux

package main

import "errors"

type sandboxNetwork struct{ path string }

func createSandboxNetwork(string, string) (*sandboxNetwork, error) {
	return nil, errors.New("sandbox network requires Linux")
}
func (*sandboxNetwork) cleanup() {}
