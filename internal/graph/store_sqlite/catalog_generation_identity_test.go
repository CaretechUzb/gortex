package store_sqlite

import (
	"context"
	"errors"
	"testing"
)

// seedIdentityGeneration begins a generation for one identity and leaves it in
// the given state. It is the fixture both tests below drive: the identity
// lookup is only meaningful against rows that went through the real begin, so
// nothing here writes the row directly.
func seedIdentityGeneration(
	t testing.TB, store *Store, identity PayloadGenerationRequest, state ViewGenerationState,
) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, handle, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, identity)
	if err != nil {
		t.Fatalf("begin generation for %s: %v", identity.TreeOID, err)
	}
	if adopted {
		t.Fatalf("generation for %s adopted %d instead of minting its own", identity.TreeOID, generationID)
	}
	if handle != nil {
		_ = handle.Close()
	}
	if state == ViewGenerationBuilding {
		return generationID
	}
	if err := store.Catalog().SetViewGenerationState(
		ctx, generationID, state, ViewGenerationBuilding,
	); err != nil {
		t.Fatalf("move generation %d to %s: %v", generationID, state, err)
	}
	return generationID
}

// TestFindViewGenerationByIdentityMatchesTheWholeIdentity is the contract the
// coordinator's restart-surviving reuse rests on: a hit is a generation the
// asking build would have produced column for column, and anything less is a
// miss. A payload built for a different tree, or one that is not in a state a
// route may name, must not be handed back — re-routing either would serve a
// checkout content it was never at.
func TestFindViewGenerationByIdentityMatchesTheWholeIdentity(t *testing.T) {
	store, identity := payloadLifecycleRaceStore(t, "catalog-identity")
	ctx := context.Background()
	catalog := store.Catalog()

	ready := seedIdentityGeneration(t, store, identity, ViewGenerationReady)

	found, ok, err := catalog.FindViewGenerationByIdentity(ctx, identity)
	if err != nil {
		t.Fatalf("FindViewGenerationByIdentity: %v", err)
	}
	if !ok || found.GenerationID != ready {
		t.Fatalf("the ready generation %d was not found: ok=%t got=%d", ready, ok, found.GenerationID)
	}

	// One column of the identity differs, so the stored payload describes a
	// different state and must not be offered for this one.
	otherTree := identity
	otherTree.TreeOID = identity.TreeOID + "-other"
	if _, ok, err := catalog.FindViewGenerationByIdentity(ctx, otherTree); err != nil || ok {
		t.Fatalf("a generation for another tree was offered: ok=%t err=%v", ok, err)
	}

	// A build still in flight is not something a route may name.
	building := identity
	building.TreeOID = identity.TreeOID + "-building"
	seedIdentityGeneration(t, store, building, ViewGenerationBuilding)
	if _, ok, err := catalog.FindViewGenerationByIdentity(ctx, building); err != nil || ok {
		t.Fatalf("an in-flight generation was offered: ok=%t err=%v", ok, err)
	}

	// Superseded says only that something newer exists; the route decides what
	// a checkout reads, so a superseded generation is still servable.
	if err := catalog.SetViewGenerationState(ctx, ready, ViewGenerationSuperseded); err != nil {
		t.Fatalf("supersede %d: %v", ready, err)
	}
	found, ok, err = catalog.FindViewGenerationByIdentity(ctx, identity)
	if err != nil || !ok || found.GenerationID != ready {
		t.Fatalf("the superseded generation was not offered: ok=%t got=%d err=%v", ok, found.GenerationID, err)
	}

	// A retiring one is on its way out and must not be re-routed.
	if err := catalog.SetViewGenerationState(ctx, ready, ViewGenerationRetiring); err != nil {
		t.Fatalf("retire %d: %v", ready, err)
	}
	if _, ok, err := catalog.FindViewGenerationByIdentity(ctx, identity); err != nil || ok {
		t.Fatalf("a retiring generation was offered: ok=%t err=%v", ok, err)
	}
}

// TestSetViewGenerationCompletenessWritesOnASealedGeneration pins the one
// property the write needs: it lands on a generation that has already
// published. What it records is a statement ABOUT the payload, discovered by
// the build that produced it, so a row guarded on the building state could
// never carry it.
func TestSetViewGenerationCompletenessWritesOnASealedGeneration(t *testing.T) {
	store, identity := payloadLifecycleRaceStore(t, "catalog-completeness")
	ctx := context.Background()
	catalog := store.Catalog()

	generationID := seedIdentityGeneration(t, store, identity, ViewGenerationReady)
	row, ok, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !ok {
		t.Fatalf("read generation %d: ok=%t err=%v", generationID, ok, err)
	}
	if row.Completeness != ViewGenerationCompleteTag {
		t.Fatalf("a fresh generation records completeness %q, want the empty default", row.Completeness)
	}

	if err := catalog.SetViewGenerationCompleteness(
		ctx, generationID, ViewGenerationClosureTruncated,
	); err != nil {
		t.Fatalf("SetViewGenerationCompleteness: %v", err)
	}
	row, _, err = catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("re-read generation %d: %v", generationID, err)
	}
	if row.Completeness != ViewGenerationClosureTruncated {
		t.Fatalf("generation %d records completeness %q, want %q",
			generationID, row.Completeness, ViewGenerationClosureTruncated)
	}
	if row.State != ViewGenerationReady {
		t.Fatalf("the write moved the generation to %q", row.State)
	}

	// Two builds of one identity reach the same verdict; the second must not
	// have to treat agreement as a failure.
	if err := catalog.SetViewGenerationCompleteness(
		ctx, generationID, ViewGenerationClosureTruncated,
	); err != nil {
		t.Fatalf("re-stating the same completeness failed: %v", err)
	}

	err = catalog.SetViewGenerationCompleteness(ctx, generationID+1000, ViewGenerationClosureTruncated)
	if !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("a write at a generation that does not exist reported %v, want ErrCatalogNotFound", err)
	}
}
