package state

import (
	"path/filepath"
	"testing"
)

func claimPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.lock")
}

func TestAClaimIsExclusive(t *testing.T) {
	path := claimPath(t)
	held, ok, err := Acquire(path)
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v)", ok, err)
	}
	defer held.Release()

	second, ok, err := Acquire(path)

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if ok {
		second.Release()
		t.Fatal("a claim already held must not be handed out twice")
	}
}

func TestAReleasedClaimIsFree(t *testing.T) {
	path := claimPath(t)
	held, _, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	next, ok, err := Acquire(path)

	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v), want the claim free again", ok, err)
	}
	next.Release()
}

func TestReleasingNothingIsHarmless(t *testing.T) {
	var claim *Claim

	if err := claim.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
