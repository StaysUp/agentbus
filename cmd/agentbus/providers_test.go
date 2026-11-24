package main

import (
	"slices"
	"testing"

	"github.com/staysup/agentbus/credentials"
	"github.com/staysup/agentbus/harness"
)

// Every provider's key can be set in Settings: credentials, which imports
// nothing of the module, lists the names by hand.
func TestProviderKeysInSettings(t *testing.T) {
	for _, p := range harness.Providers {
		if !slices.Contains(credentials.KeyNames, p.Key) || p.KeyLabel == "" {
			t.Errorf("%s: key %q is not one Settings keeps, or has no label", p.Name, p.Key)
		}
	}
}
