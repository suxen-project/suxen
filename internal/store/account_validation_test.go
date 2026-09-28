package store

import (
	"context"
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestAccountWritesRejectInvalidNamesAndMissingPasswords(t *testing.T) {
	metadata := openCompanionTestStore(t, "sqlite")
	ctx := context.Background()
	for _, username := range []string{"", "ops/team", "ops:team", " ops", ".."} {
		if err := metadata.SaveUser(ctx, UserSave{Username: username, Password: "secret", Create: true}); !errors.Is(err, domain.ErrInvalidUsername) {
			t.Errorf("SaveUser(%q) = %v, want ErrInvalidUsername", username, err)
		}
		if err := metadata.CreateUser(ctx, username, "secret", false); !errors.Is(err, domain.ErrInvalidUsername) {
			t.Errorf("CreateUser(%q) = %v, want ErrInvalidUsername", username, err)
		}
		if err := metadata.CreateBootstrapAdmin(ctx, username, "secret", "token"); !errors.Is(err, domain.ErrInvalidUsername) {
			t.Errorf("CreateBootstrapAdmin(%q) = %v, want ErrInvalidUsername", username, err)
		}
	}
	if err := metadata.SaveUser(ctx, UserSave{Username: "operator", Create: true}); !errors.Is(err, domain.ErrPasswordRequired) {
		t.Errorf("SaveUser missing password = %v, want ErrPasswordRequired", err)
	}
	if err := metadata.CreateUser(ctx, "operator", "", false); !errors.Is(err, domain.ErrPasswordRequired) {
		t.Errorf("CreateUser missing password = %v, want ErrPasswordRequired", err)
	}
	if err := metadata.CreateBootstrapAdmin(ctx, "operator", "", "token"); !errors.Is(err, domain.ErrPasswordRequired) {
		t.Errorf("CreateBootstrapAdmin missing password = %v, want ErrPasswordRequired", err)
	}
}
