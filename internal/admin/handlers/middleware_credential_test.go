package handlers

import (
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

func TestCredentialValidator(t *testing.T) {
	ctx := t.Context()
	store := repository.NewKeyStore()
	key, err := store.Create(ctx, "worker", []string{model.ScopeReadOnly}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	validate, _ := NewCredentialValidator(store, "master-secret")

	t.Run("master key resolves to the master identity", func(t *testing.T) {
		got, err := validate(ctx, "master-secret")
		if err != nil {
			t.Fatalf("master key rejected: %v", err)
		}
		if want := masterCredentialID("master-secret"); got.ID != want {
			t.Fatalf("ID = %q, want %q", got.ID, want)
		}
		if len(got.Scopes) != 1 || got.Scopes[0] != model.ScopeAdmin {
			t.Fatalf("scopes = %v, want [admin]", got.Scopes)
		}
	})

	t.Run("stored key resolves with its own scopes", func(t *testing.T) {
		got, err := validate(ctx, key.Key)
		if err != nil {
			t.Fatalf("stored key rejected: %v", err)
		}
		if len(got.Scopes) != 1 || got.Scopes[0] != model.ScopeReadOnly {
			t.Fatalf("scopes = %v, want [read_only]", got.Scopes)
		}
	})

	t.Run("unknown credential is rejected", func(t *testing.T) {
		if _, err := validate(ctx, "nonsense"); !errors.Is(err, model.ErrInvalidCredential) {
			t.Fatalf("unknown credential: err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("empty credential is rejected", func(t *testing.T) {
		if _, err := validate(ctx, ""); !errors.Is(err, model.ErrInvalidCredential) {
			t.Fatalf("empty credential: err = %v, want ErrInvalidCredential", err)
		}
	})

	t.Run("master key wins over an identical stored key", func(t *testing.T) {
		s := repository.NewKeyStore()
		k, err := s.Create(ctx, "collide", []string{model.ScopeReadOnly}, nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		collideValidate, _ := NewCredentialValidator(s, k.Key)
		got, err := collideValidate(ctx, k.Key)
		if err != nil {
			t.Fatalf("credential rejected: %v", err)
		}
		if want := masterCredentialID(k.Key); got.ID != want {
			t.Fatalf("ID = %q, want %q: the store was consulted before the master key", got.ID, want)
		}
	})
}
