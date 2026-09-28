package controlplane

import "github.com/suxen-project/suxen/internal/store"

// Intent selects the ownership policy applied to a command. An imperative API
// mutation may only seize a provisioning-managed resource with Force; a
// declarative reconciliation adopts or refreshes the ownership record and
// persists the secret Fingerprint.
type Intent struct {
	Declarative bool
	Fingerprint string
	Force       bool
}

// Imperative is the intent of an HTTP API caller. Force transfers ownership away
// from declarative provisioning.
func Imperative(force bool) Intent { return Intent{Force: force} }

// Declarative is the intent of a provisioning reconciliation carrying the secret
// fingerprint to persist with the ownership record.
func Declarative(fingerprint string) Intent {
	return Intent{Declarative: true, Fingerprint: fingerprint}
}

func (intent Intent) ownership() store.Ownership {
	return store.Ownership{
		Declarative: intent.Declarative,
		Fingerprint: intent.Fingerprint,
		Force:       intent.Force,
	}
}
