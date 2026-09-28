package provision

import (
	"context"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProvisionRepositoryOverwriteCreatePreserveAndOverlay(t *testing.T) {
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	ctx := context.Background()
	apply := func(document string) {
		t.Helper()
		report, err := engine.Apply(ctx, mustResolveDocument(t, document), Options{})
		if err != nil || report.Failed() {
			t.Fatalf("apply: report=%+v err=%v", report, err)
		}
	}
	apply(`resources:
  - kind: repository
    name: hosted
    spec:
      format: raw
      type: hosted
      allowOverwrite: false
`)
	check := func(want bool) domain.Repository {
		t.Helper()
		stored, err := metadata.Repository(ctx, "hosted")
		if err != nil {
			t.Fatal(err)
		}
		if stored.AllowOverwrite == nil || *stored.AllowOverwrite != want {
			t.Fatalf("stored policy = %v, want %t", stored.AllowOverwrite, want)
		}
		if got, ok := repositoryMap(stored)["allowOverwrite"].(bool); !ok || got != want {
			t.Fatalf("repository map policy = %v", repositoryMap(stored)["allowOverwrite"])
		}
		return stored
	}
	check(false)
	apply(`resources:
  - kind: repository
    name: hosted
    spec:
      format: raw
      type: hosted
`)
	check(false)
	apply(`resources:
  - kind: repository
    name: hosted
    spec:
      format: raw
      type: hosted
      allowOverwrite: true
`)
	check(true)
}
