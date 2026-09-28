package store

import "context"

// OwnershipRecords is the declarative-provisioning ownership-record capability:
// read one or all records, and create/replace or forget one. This slice routes
// the engine's four record call sites through this narrow port; the engine still
// holds the full Store for per-kind reconcile/prune reads, and records() falls
// back to it. So this port documents and isolates the ownership-record capability
// but does not by itself enforce atomicity or block other store mutations — the
// engine can still write a record separately when a reconciler reports its
// mutation did not fold the record (ownershipPersisted false). The stronger
// guarantee (the record written only inside the per-resource transaction) comes
// from the feature command ports and the later Engine.Store per-kind split.
type OwnershipRecords interface {
	ProvisionRecord(context.Context, string, string) (ProvisionRecord, error)
	ProvisionRecords(context.Context) ([]ProvisionRecord, error)
	PutProvisionRecord(context.Context, ProvisionRecord) error
	DeleteProvisionRecord(context.Context, string, string) error
}
