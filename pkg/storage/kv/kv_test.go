package kv

import (
	"context"
	"testing"
	"time"

	"github.com/glidea/zenfeed/pkg/component"
	"github.com/glidea/zenfeed/pkg/config"
)

func TestOperationsBeforeRunReturnNotReady(t *testing.T) {
	t.Parallel()

	storage, err := NewFactory().New(component.Global, &config.App{}, Dependencies{})
	if err != nil {
		t.Fatalf("new kv storage: %v", err)
	}

	ctx := context.Background()
	if _, err := storage.Get(ctx, []byte("test")); err != errDBNotReady {
		t.Fatalf("Get err = %v, want %v", err, errDBNotReady)
	}
	if err := storage.Set(ctx, []byte("test"), []byte("value"), time.Minute); err != errDBNotReady {
		t.Fatalf("Set err = %v, want %v", err, errDBNotReady)
	}
	if err := storage.Delete(ctx, []byte("test")); err != errDBNotReady {
		t.Fatalf("Delete err = %v, want %v", err, errDBNotReady)
	}
	if _, err := storage.Keys(ctx, []byte("test")); err != errDBNotReady {
		t.Fatalf("Keys err = %v, want %v", err, errDBNotReady)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("Close err = %v, want nil", err)
	}
}
