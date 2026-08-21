package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// Catalog is the accessor for the checkout-lifecycle control plane — the
// families, checkouts, tracking intents, dedicated graphs, view generations,
// routes, ref views and cleanup work described by checkoutCatalogSchemaSQL.
//
// It is a separate handle rather than another few dozen methods on Store
// because none of it is graph payload: no call here reads or writes nodes,
// edges, files or their sidecars, and none of it participates in the
// analysis-generation invalidation that every payload mutation must run.
//
// Writes take the store's mutation gate and run on the active writer
// connection, so they serialise with graph writes exactly like every other
// durable write in this package. Reads go to the read pool.
type Catalog struct {
	store *Store
}

// Catalog returns the control-plane accessor for this store.
func (s *Store) Catalog() *Catalog { return &Catalog{store: s} }

// exec runs one control-plane statement under the mutation gate.
func (c *Catalog) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.store.writeMu.Lock()
	defer c.store.writeMu.Unlock()
	return c.store.execActiveWriteLocked(ctx, query, args...)
}

// execGuarded runs one compare-and-set statement and reports a no-op as a
// stale guard. A lone UPDATE is already its own transaction in SQLite, so the
// read of the guard columns and the write cannot be interleaved.
func (c *Catalog) execGuarded(ctx context.Context, subject string, query string, args ...any) error {
	result, err := c.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
	}
	return nil
}

// deleteOne runs a delete addressed at a single row and reports a no-op as
// ErrCatalogNotFound, so a caller can tell "it was already gone" from "the
// statement failed" without inspecting driver errors.
func (c *Catalog) deleteOne(ctx context.Context, subject string, query string, args ...any) error {
	result, err := c.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogNotFound, subject)
	}
	return nil
}

// withTx runs a multi-statement guarded transition as one transaction under
// the mutation gate.
func (c *Catalog) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	c.store.writeMu.Lock()
	defer c.store.writeMu.Unlock()
	tx, err := c.store.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// catalogNullString stores the empty string as NULL, so "unset" and "set to
// empty" cannot be confused by a partial index or a uniqueness rule.
func catalogNullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// catalogNullInt stores 0 as NULL for the generation pointers, whose zero
// value means "no generation".
func catalogNullInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func catalogBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// --- repository families ----------------------------------------------

// UpsertRepositoryFamily writes one family row. It never uses INSERT OR
// REPLACE: REPLACE deletes the existing row first, which would cascade
// through every checkout that references it.
func (c *Catalog) UpsertRepositoryFamily(ctx context.Context, family RepositoryFamily) error {
	if err := family.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO repository_families
  (family_id, common_dir_identity, display_remote, state, primary_epoch, created_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(family_id) DO UPDATE SET
  common_dir_identity = excluded.common_dir_identity,
  display_remote      = excluded.display_remote,
  state               = excluded.state,
  primary_epoch       = excluded.primary_epoch,
  created_at          = excluded.created_at,
  last_seen           = excluded.last_seen`,
		family.FamilyID, family.CommonDirIdentity, family.DisplayRemote, family.State,
		family.PrimaryEpoch, family.CreatedAt, family.LastSeen)
	return err
}

// GetRepositoryFamily returns one family. The bool is false when no row exists.
func (c *Catalog) GetRepositoryFamily(ctx context.Context, familyID string) (RepositoryFamily, bool, error) {
	family := RepositoryFamily{FamilyID: familyID}
	err := c.store.db.QueryRowContext(ctx, `
SELECT common_dir_identity, display_remote, state, primary_epoch, created_at, last_seen
  FROM repository_families WHERE family_id = ?`, familyID).Scan(
		&family.CommonDirIdentity, &family.DisplayRemote, &family.State,
		&family.PrimaryEpoch, &family.CreatedAt, &family.LastSeen)
	if err == sql.ErrNoRows {
		return RepositoryFamily{}, false, nil
	}
	if err != nil {
		return RepositoryFamily{}, false, err
	}
	return family, true, nil
}

// DeleteRepositoryFamily removes a family. Checkouts and dedicated graphs
// reference it with ON DELETE RESTRICT, so SQLite refuses the delete until the
// family is empty — the row is the last thing a family teardown removes.
func (c *Catalog) DeleteRepositoryFamily(ctx context.Context, familyID string) error {
	if err := requireCatalogID("family_id", familyID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("family %s", familyID),
		`DELETE FROM repository_families WHERE family_id = ?`, familyID)
}

// --- checkouts ---------------------------------------------------------

const checkoutColumns = `incarnation, family_id, root_path, git_dir, admin_name, state,
	desired_mode, effective_mode, locked, prunable, head_ref, head_commit, head_tree,
	last_accessible, unavailable_since, availability_deadline, removal_detected_at,
	removal_deadline, removal_evidence, active_intent_transition_id, last_seen, last_error`

// UpsertCheckout writes one checkout row.
func (c *Catalog) UpsertCheckout(ctx context.Context, checkout Checkout) error {
	if err := checkout.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO checkouts (checkout_id, `+checkoutColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  incarnation                 = excluded.incarnation,
  family_id                   = excluded.family_id,
  root_path                   = excluded.root_path,
  git_dir                     = excluded.git_dir,
  admin_name                  = excluded.admin_name,
  state                       = excluded.state,
  desired_mode                = excluded.desired_mode,
  effective_mode              = excluded.effective_mode,
  locked                      = excluded.locked,
  prunable                    = excluded.prunable,
  head_ref                    = excluded.head_ref,
  head_commit                 = excluded.head_commit,
  head_tree                   = excluded.head_tree,
  last_accessible             = excluded.last_accessible,
  unavailable_since           = excluded.unavailable_since,
  availability_deadline       = excluded.availability_deadline,
  removal_detected_at         = excluded.removal_detected_at,
  removal_deadline            = excluded.removal_deadline,
  removal_evidence            = excluded.removal_evidence,
  active_intent_transition_id = excluded.active_intent_transition_id,
  last_seen                   = excluded.last_seen,
  last_error                  = excluded.last_error`,
		checkout.CheckoutID, checkout.Incarnation, checkout.FamilyID, checkout.RootPath,
		checkout.GitDir, checkout.AdminName, string(checkout.State),
		string(checkout.DesiredMode), string(checkout.EffectiveMode),
		catalogBoolInt(checkout.Locked), catalogBoolInt(checkout.Prunable),
		checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree,
		checkout.LastAccessible, checkout.UnavailableSince, checkout.AvailabilityDeadline,
		checkout.RemovalDetectedAt, checkout.RemovalDeadline, checkout.RemovalEvidence,
		catalogNullString(checkout.ActiveIntentTransitionID),
		checkout.LastSeen, checkout.LastError)
	return err
}

// AllocateCheckout mints the identity for a working copy the catalog has never
// seen. Unlike UpsertCheckout it refuses to add a second live identity for a
// (family_id, admin_name) that already has one: the insert carries its own
// existence test, so two actors racing to allocate the same working copy end
// with one row, and the loser gets ErrCatalogStaleGuard.
//
// The table's UNIQUE key cannot serve as that backstop — it includes the
// incarnation precisely so a removed-and-recreated path can be re-keyed under
// the same name — and the test is written into the statement rather than run
// as a read before it, because a separate read would leave open the very
// window it is meant to close.
func (c *Catalog) AllocateCheckout(ctx context.Context, checkout Checkout) error {
	if err := checkout.validate(); err != nil {
		return err
	}
	// The guard is keyed on the administrative name, so an allocation without
	// one cannot be guarded at all.
	if err := requireCatalogID("admin_name", checkout.AdminName); err != nil {
		return err
	}
	subject := fmt.Sprintf("family %s already holds admin name %s", checkout.FamilyID, checkout.AdminName)
	return c.execGuarded(ctx, subject, `
INSERT INTO checkouts (checkout_id, `+checkoutColumns+`)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM checkouts WHERE family_id = ? AND admin_name = ?)`,
		checkout.CheckoutID, checkout.Incarnation, checkout.FamilyID, checkout.RootPath,
		checkout.GitDir, checkout.AdminName, string(checkout.State),
		string(checkout.DesiredMode), string(checkout.EffectiveMode),
		catalogBoolInt(checkout.Locked), catalogBoolInt(checkout.Prunable),
		checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree,
		checkout.LastAccessible, checkout.UnavailableSince, checkout.AvailabilityDeadline,
		checkout.RemovalDetectedAt, checkout.RemovalDeadline, checkout.RemovalEvidence,
		catalogNullString(checkout.ActiveIntentTransitionID),
		checkout.LastSeen, checkout.LastError,
		checkout.FamilyID, checkout.AdminName)
}

// scanCheckout reads the checkoutColumns projection in order.
func scanCheckout(scan func(...any) error, checkout *Checkout) error {
	var (
		state, desiredMode, effectiveMode string
		locked, prunable                  int
		activeTransition                  sql.NullString
	)
	if err := scan(
		&checkout.Incarnation, &checkout.FamilyID, &checkout.RootPath, &checkout.GitDir,
		&checkout.AdminName, &state, &desiredMode, &effectiveMode, &locked, &prunable,
		&checkout.HeadRef, &checkout.HeadCommit, &checkout.HeadTree,
		&checkout.LastAccessible, &checkout.UnavailableSince, &checkout.AvailabilityDeadline,
		&checkout.RemovalDetectedAt, &checkout.RemovalDeadline, &checkout.RemovalEvidence,
		&activeTransition, &checkout.LastSeen, &checkout.LastError); err != nil {
		return err
	}
	checkout.State = CheckoutState(state)
	checkout.DesiredMode = CheckoutMode(desiredMode)
	checkout.EffectiveMode = CheckoutMode(effectiveMode)
	checkout.Locked = locked != 0
	checkout.Prunable = prunable != 0
	checkout.ActiveIntentTransitionID = activeTransition.String
	return nil
}

// GetCheckout returns one checkout. The bool is false when no row exists.
func (c *Catalog) GetCheckout(ctx context.Context, checkoutID string) (Checkout, bool, error) {
	checkout := Checkout{CheckoutID: checkoutID}
	row := c.store.db.QueryRowContext(ctx, `SELECT `+checkoutColumns+` FROM checkouts WHERE checkout_id = ?`, checkoutID)
	err := scanCheckout(row.Scan, &checkout)
	if err == sql.ErrNoRows {
		return Checkout{}, false, nil
	}
	if err != nil {
		return Checkout{}, false, err
	}
	return checkout, true, nil
}

// ListCheckouts returns one family's checkouts. The scan rides the
// UNIQUE(family_id, admin_name, incarnation) index, so it is bounded by the
// family rather than by the table.
func (c *Catalog) ListCheckouts(ctx context.Context, familyID string) ([]Checkout, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT checkout_id, `+checkoutColumns+`
  FROM checkouts WHERE family_id = ? ORDER BY admin_name, incarnation`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkout
	for rows.Next() {
		var checkout Checkout
		err := scanCheckout(func(dest ...any) error {
			return rows.Scan(append([]any{&checkout.CheckoutID}, dest...)...)
		}, &checkout)
		if err != nil {
			return nil, err
		}
		out = append(out, checkout)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateCheckoutState is the guarded checkout-state transition: it applies
// only when the stored incarnation still matches the caller's expectation, so
// a write aimed at a working copy that has since been removed and recreated
// changes nothing and reports ErrCatalogStaleGuard.
func (c *Catalog) UpdateCheckoutState(ctx context.Context, req UpdateCheckoutStateRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", req.Incarnation); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, checkoutStates); err != nil {
		return err
	}
	if err := requireCatalogValue("desired_mode", req.DesiredMode, checkoutModes); err != nil {
		return err
	}
	if err := requireCatalogValue("effective_mode", req.EffectiveMode, checkoutModes); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s incarnation %s", req.CheckoutID, req.Incarnation), `
UPDATE checkouts
   SET state = ?, desired_mode = ?, effective_mode = ?, last_seen = ?, last_error = ?
 WHERE checkout_id = ? AND incarnation = ?`,
		string(req.State), string(req.DesiredMode), string(req.EffectiveMode),
		req.LastSeen, req.LastError, req.CheckoutID, req.Incarnation)
}

// UpdateCheckoutObservation is the guarded write a reconciliation pass makes
// after looking at a checkout: it moves the state axis, both durable clock
// axes and the observed git / filesystem facts in one statement, under the
// same incarnation guard UpdateCheckoutState uses.
//
// It exists beside UpdateCheckoutState because the two answer different
// questions. UpdateCheckoutState is the mode-transition write and touches only
// what a promotion or demotion changes. This is the observation write, and the
// clocks have to land in the same statement as the state they justify: split
// across two statements, a crash between them leaves a state whose deadline
// says something else. The identity columns (checkout_id, incarnation,
// family_id, admin_name) are deliberately absent — an observation never
// re-keys the row it observed.
//
// The two mode columns are absent for a different reason. An observer reads
// what git and the filesystem say; it has nothing to say about how a checkout
// is served. Writing back the modes it happened to read would let a pass whose
// read predates a promotion revert it, because the incarnation guard does not
// move on a mode transition. The two writers touch disjoint columns instead,
// so neither can lose the other's update.
func (c *Catalog) UpdateCheckoutObservation(ctx context.Context, req UpdateCheckoutObservationRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s incarnation %s", req.CheckoutID, req.Incarnation), `
UPDATE checkouts
   SET state = ?,
       root_path = ?, git_dir = ?, locked = ?, prunable = ?,
       head_ref = ?, head_commit = ?, head_tree = ?,
       last_accessible = ?, unavailable_since = ?, availability_deadline = ?,
       removal_detected_at = ?, removal_deadline = ?, removal_evidence = ?,
       last_seen = ?, last_error = ?
 WHERE checkout_id = ? AND incarnation = ?`,
		string(req.State),
		req.RootPath, req.GitDir, catalogBoolInt(req.Locked), catalogBoolInt(req.Prunable),
		req.HeadRef, req.HeadCommit, req.HeadTree,
		req.LastAccessible, req.UnavailableSince, req.AvailabilityDeadline,
		req.RemovalDetectedAt, req.RemovalDeadline, req.RemovalEvidence,
		req.LastSeen, req.LastError, req.CheckoutID, req.Incarnation)
}

// DeleteCheckout removes a checkout. Its tracking intents, in-flight intent
// transition and path evidence go with it through ON DELETE CASCADE; a route
// does not cascade, so a routed checkout must be un-routed first and SQLite
// refuses the delete until then.
func (c *Catalog) DeleteCheckout(ctx context.Context, checkoutID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("checkout %s", checkoutID),
		`DELETE FROM checkouts WHERE checkout_id = ?`, checkoutID)
}

// --- tracking intents --------------------------------------------------

// UpsertTrackingIntent writes one tracking intent. A repeated request from the
// same source for the same checkout updates the existing row rather than
// adding a duplicate.
func (c *Catalog) UpsertTrackingIntent(ctx context.Context, intent TrackingIntent) error {
	if err := intent.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO tracking_intents
  (intent_id, checkout_id, source_kind, source_locator, active, created_at, revoked_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id, source_kind, source_locator) DO UPDATE SET
  active     = excluded.active,
  revoked_at = excluded.revoked_at,
  last_error = excluded.last_error`,
		intent.IntentID, intent.CheckoutID, string(intent.SourceKind), intent.SourceLocator,
		catalogBoolInt(intent.Active), intent.CreatedAt, intent.RevokedAt, intent.LastError)
	return err
}

// ListTrackingIntents returns one checkout's intents, riding the
// UNIQUE(checkout_id, source_kind, source_locator) index.
func (c *Catalog) ListTrackingIntents(ctx context.Context, checkoutID string) ([]TrackingIntent, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT intent_id, source_kind, source_locator, active, created_at, revoked_at, last_error
  FROM tracking_intents WHERE checkout_id = ? ORDER BY source_kind, source_locator`, checkoutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackingIntent
	for rows.Next() {
		intent := TrackingIntent{CheckoutID: checkoutID}
		var (
			sourceKind string
			active     int
		)
		if err := rows.Scan(&intent.IntentID, &sourceKind, &intent.SourceLocator, &active,
			&intent.CreatedAt, &intent.RevokedAt, &intent.LastError); err != nil {
			return nil, err
		}
		intent.SourceKind = IntentSourceKind(sourceKind)
		intent.Active = active != 0
		out = append(out, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// --- intent transitions ------------------------------------------------

// BeginIntentTransition records the single in-flight mode change for a
// checkout and points the checkout row at it, in one transaction. A checkout
// that already has one reports ErrCatalogIntentTransitionActive and nothing is
// written — UNIQUE(checkout_id) is the enforcement, the pre-check only turns
// it into a typed error.
func (c *Catalog) BeginIntentTransition(ctx context.Context, transition IntentTransition) error {
	if err := transition.validate(); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT transition_id FROM intent_transitions WHERE checkout_id = ?`,
			transition.CheckoutID).Scan(&existing)
		switch {
		case err == nil:
			return fmt.Errorf("%w: checkout %s holds transition %s",
				ErrCatalogIntentTransitionActive, transition.CheckoutID, existing)
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO intent_transitions
  (transition_id, checkout_id, cause, prior_desired_mode, prior_effective_mode,
   requested_mode, prior_checkout_state, source_snapshot_hash, state,
   created_at, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			transition.TransitionID, transition.CheckoutID, transition.Cause,
			catalogNullString(string(transition.PriorDesiredMode)),
			catalogNullString(string(transition.PriorEffectiveMode)),
			catalogNullString(string(transition.RequestedMode)),
			catalogNullString(string(transition.PriorCheckoutState)),
			catalogNullString(transition.SourceSnapshotHash),
			string(transition.State), transition.CreatedAt,
			transition.LastProgress, transition.LastError); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE checkouts SET active_intent_transition_id = ? WHERE checkout_id = ?`,
			transition.TransitionID, transition.CheckoutID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: checkout %s", ErrCatalogNotFound, transition.CheckoutID)
		}
		return nil
	})
}

// GetIntentTransition returns a checkout's in-flight transition. The bool is
// false when the checkout has none.
func (c *Catalog) GetIntentTransition(ctx context.Context, checkoutID string) (IntentTransition, bool, error) {
	transition := IntentTransition{CheckoutID: checkoutID}
	var (
		priorDesired, priorEffective, requested sql.NullString
		priorState, snapshotHash                sql.NullString
		state                                   string
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT transition_id, cause, prior_desired_mode, prior_effective_mode, requested_mode,
       prior_checkout_state, source_snapshot_hash, state, created_at, last_progress, last_error
  FROM intent_transitions WHERE checkout_id = ?`, checkoutID).Scan(
		&transition.TransitionID, &transition.Cause, &priorDesired, &priorEffective,
		&requested, &priorState, &snapshotHash, &state,
		&transition.CreatedAt, &transition.LastProgress, &transition.LastError)
	if err == sql.ErrNoRows {
		return IntentTransition{}, false, nil
	}
	if err != nil {
		return IntentTransition{}, false, err
	}
	transition.PriorDesiredMode = CheckoutMode(priorDesired.String)
	transition.PriorEffectiveMode = CheckoutMode(priorEffective.String)
	transition.RequestedMode = CheckoutMode(requested.String)
	transition.PriorCheckoutState = CheckoutState(priorState.String)
	transition.SourceSnapshotHash = snapshotHash.String
	transition.State = IntentTransitionState(state)
	return transition, true, nil
}

// CompleteIntentTransition releases the transition slot: it deletes the row
// and clears the checkout's pointer in one transaction. The delete is guarded
// by both ids, so a caller holding a stale transition id cannot release a
// transition that replaced its own.
func (c *Catalog) CompleteIntentTransition(ctx context.Context, checkoutID, transitionID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("transition_id", transitionID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM intent_transitions WHERE transition_id = ? AND checkout_id = ?`,
			transitionID, checkoutID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: transition %s on checkout %s", ErrCatalogStaleGuard, transitionID, checkoutID)
		}
		_, err = tx.ExecContext(ctx, `
UPDATE checkouts SET active_intent_transition_id = NULL
 WHERE checkout_id = ? AND active_intent_transition_id = ?`, checkoutID, transitionID)
		return err
	})
}

// --- checkout path evidence --------------------------------------------

// UpsertCheckoutPathEvidence replaces a checkout's filesystem sample.
func (c *Catalog) UpsertCheckoutPathEvidence(ctx context.Context, evidence CheckoutPathEvidence) error {
	if err := requireCatalogID("checkout_id", evidence.CheckoutID); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO checkout_path_evidence
  (checkout_id, root_path_identity, root_volume_kind, root_volume_token,
   nearest_existing_ancestor_path, ancestor_volume_kind, ancestor_volume_token,
   common_dir_volume_kind, common_dir_volume_token, sampled_at, sample_generation)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  root_path_identity             = excluded.root_path_identity,
  root_volume_kind               = excluded.root_volume_kind,
  root_volume_token              = excluded.root_volume_token,
  nearest_existing_ancestor_path = excluded.nearest_existing_ancestor_path,
  ancestor_volume_kind           = excluded.ancestor_volume_kind,
  ancestor_volume_token          = excluded.ancestor_volume_token,
  common_dir_volume_kind         = excluded.common_dir_volume_kind,
  common_dir_volume_token        = excluded.common_dir_volume_token,
  sampled_at                     = excluded.sampled_at,
  sample_generation              = excluded.sample_generation`,
		evidence.CheckoutID, evidence.RootPathIdentity, evidence.RootVolumeKind,
		evidence.RootVolumeToken, evidence.NearestExistingAncestorPath,
		evidence.AncestorVolumeKind, evidence.AncestorVolumeToken,
		evidence.CommonDirVolumeKind, evidence.CommonDirVolumeToken,
		evidence.SampledAt, evidence.SampleGeneration)
	return err
}

// GetCheckoutPathEvidence returns a checkout's last filesystem sample.
func (c *Catalog) GetCheckoutPathEvidence(ctx context.Context, checkoutID string) (CheckoutPathEvidence, bool, error) {
	evidence := CheckoutPathEvidence{CheckoutID: checkoutID}
	err := c.store.db.QueryRowContext(ctx, `
SELECT root_path_identity, root_volume_kind, root_volume_token,
       nearest_existing_ancestor_path, ancestor_volume_kind, ancestor_volume_token,
       common_dir_volume_kind, common_dir_volume_token, sampled_at, sample_generation
  FROM checkout_path_evidence WHERE checkout_id = ?`, checkoutID).Scan(
		&evidence.RootPathIdentity, &evidence.RootVolumeKind, &evidence.RootVolumeToken,
		&evidence.NearestExistingAncestorPath, &evidence.AncestorVolumeKind,
		&evidence.AncestorVolumeToken, &evidence.CommonDirVolumeKind,
		&evidence.CommonDirVolumeToken, &evidence.SampledAt, &evidence.SampleGeneration)
	if err == sql.ErrNoRows {
		return CheckoutPathEvidence{}, false, nil
	}
	if err != nil {
		return CheckoutPathEvidence{}, false, err
	}
	return evidence, true, nil
}

// --- dedicated graphs ---------------------------------------------------

// UpsertDedicatedGraph writes one dedicated-graph row. Setting IsPrimaryBase
// here is only legal while no other graph in the family holds it — the partial
// unique index refuses a second one. Moving the flag between graphs is
// SetPrimaryDedicatedGraph's job, which clears the incumbent first.
func (c *Catalog) UpsertDedicatedGraph(ctx context.Context, dedicated DedicatedGraph) error {
	if err := dedicated.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO dedicated_graphs
  (graph_id, owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(graph_id) DO UPDATE SET
  owner_checkout_id    = excluded.owner_checkout_id,
  repo_prefix          = excluded.repo_prefix,
  family_id            = excluded.family_id,
  is_primary_base      = excluded.is_primary_base,
  active_generation_id = excluded.active_generation_id,
  state                = excluded.state`,
		dedicated.GraphID, catalogNullString(dedicated.OwnerCheckoutID), dedicated.RepoPrefix,
		dedicated.FamilyID, catalogBoolInt(dedicated.IsPrimaryBase),
		catalogNullInt(dedicated.ActiveGenerationID), dedicated.State)
	return err
}

// GetDedicatedGraph returns one dedicated graph.
func (c *Catalog) GetDedicatedGraph(ctx context.Context, graphID string) (DedicatedGraph, bool, error) {
	dedicated := DedicatedGraph{GraphID: graphID}
	var (
		owner         sql.NullString
		activeGen     sql.NullInt64
		isPrimaryBase int
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(
		&owner, &dedicated.RepoPrefix, &dedicated.FamilyID, &isPrimaryBase,
		&activeGen, &dedicated.State)
	if err == sql.ErrNoRows {
		return DedicatedGraph{}, false, nil
	}
	if err != nil {
		return DedicatedGraph{}, false, err
	}
	dedicated.OwnerCheckoutID = owner.String
	dedicated.ActiveGenerationID = activeGen.Int64
	dedicated.IsPrimaryBase = isPrimaryBase != 0
	return dedicated, true, nil
}

// ListDedicatedGraphs returns one family's dedicated graphs, ordered by graph
// id so two passes over an unchanged family see the same order. It is how a
// caller finds the family's primary base and the graph a given checkout owns,
// neither of which is addressable by a graph id it does not know yet.
func (c *Catalog) ListDedicatedGraphs(ctx context.Context, familyID string) ([]DedicatedGraph, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT graph_id, owner_checkout_id, repo_prefix, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE family_id = ? ORDER BY graph_id`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DedicatedGraph
	for rows.Next() {
		dedicated := DedicatedGraph{FamilyID: familyID}
		var (
			owner         sql.NullString
			activeGen     sql.NullInt64
			isPrimaryBase int
		)
		if err := rows.Scan(&dedicated.GraphID, &owner, &dedicated.RepoPrefix,
			&isPrimaryBase, &activeGen, &dedicated.State); err != nil {
			return nil, err
		}
		dedicated.OwnerCheckoutID = owner.String
		dedicated.ActiveGenerationID = activeGen.Int64
		dedicated.IsPrimaryBase = isPrimaryBase != 0
		out = append(out, dedicated)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteDedicatedGraph removes a dedicated-graph row. The graph's generations
// are not touched: active_generation_id is a plain integer, so a caller that
// wants them gone prunes them itself.
func (c *Catalog) DeleteDedicatedGraph(ctx context.Context, graphID string) error {
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("dedicated graph %s", graphID),
		`DELETE FROM dedicated_graphs WHERE graph_id = ?`, graphID)
}

// SetPrimaryDedicatedGraph moves the family's primary base to one graph. The
// family's primary_epoch is the compare-and-set token: a promotion carrying a
// stale epoch changes nothing and reports ErrCatalogStaleGuard, so two
// reconcilers cannot each believe they installed the primary. The incumbent is
// cleared before the new holder is set, because the partial unique index
// permits exactly one is_primary_base row per family at any point.
func (c *Catalog) SetPrimaryDedicatedGraph(ctx context.Context, req SetPrimaryDedicatedGraphRequest) error {
	if err := requireCatalogID("family_id", req.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE repository_families
   SET primary_epoch = primary_epoch + 1, last_seen = ?
 WHERE family_id = ? AND primary_epoch = ?`,
			req.LastSeen, req.FamilyID, req.ExpectedPrimaryEpoch)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: family %s primary epoch %d",
				ErrCatalogStaleGuard, req.FamilyID, req.ExpectedPrimaryEpoch)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs SET is_primary_base = 0
 WHERE family_id = ? AND is_primary_base = 1 AND graph_id <> ?`,
			req.FamilyID, req.GraphID); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `
UPDATE dedicated_graphs SET is_primary_base = 1
 WHERE graph_id = ? AND family_id = ?`, req.GraphID, req.FamilyID)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: graph %s in family %s", ErrCatalogNotFound, req.GraphID, req.FamilyID)
		}
		return nil
	})
}

// --- view generations ---------------------------------------------------

const viewGenerationColumns = `owner_kind, graph_id, layer_id, checkout_id, generation_kind,
	base_generation_id, lower_view_fingerprint, tree_oid, provenance_commit_oid, config_hash,
	extractor_versions, resolver_version, state, covered_files, affected_files, storage_bytes,
	completeness, created_at, published_at, last_selected, error`

// CreateViewGeneration inserts a generation and returns its assigned id. The
// row is written exactly once; afterwards only PublishViewGeneration may
// change it, and only out of the building state.
func (c *Catalog) CreateViewGeneration(ctx context.Context, generation ViewGeneration) (int64, error) {
	if err := generation.validate(); err != nil {
		return 0, err
	}
	result, err := c.exec(ctx, `
INSERT INTO view_generations (`+viewGenerationColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		generation.OwnerKind, generation.GraphID,
		catalogNullString(generation.LayerID), catalogNullString(generation.CheckoutID),
		generation.GenerationKind, catalogNullInt(generation.BaseGenerationID),
		generation.LowerViewFingerprint, generation.TreeOID,
		catalogNullString(generation.ProvenanceCommitOID), generation.ConfigHash,
		generation.ExtractorVersions, generation.ResolverVersion, string(generation.State),
		generation.CoveredFiles, generation.AffectedFiles, generation.StorageBytes,
		generation.Completeness, generation.CreatedAt, generation.PublishedAt,
		generation.LastSelected, generation.Error)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetViewGeneration returns one generation.
func (c *Catalog) GetViewGeneration(ctx context.Context, generationID int64) (ViewGeneration, bool, error) {
	generation := ViewGeneration{GenerationID: generationID}
	var (
		layerID, checkoutID, provenance sql.NullString
		baseGeneration                  sql.NullInt64
		state                           string
	)
	err := c.store.db.QueryRowContext(ctx,
		`SELECT `+viewGenerationColumns+` FROM view_generations WHERE generation_id = ?`,
		generationID).Scan(
		&generation.OwnerKind, &generation.GraphID, &layerID, &checkoutID,
		&generation.GenerationKind, &baseGeneration, &generation.LowerViewFingerprint,
		&generation.TreeOID, &provenance, &generation.ConfigHash,
		&generation.ExtractorVersions, &generation.ResolverVersion, &state,
		&generation.CoveredFiles, &generation.AffectedFiles, &generation.StorageBytes,
		&generation.Completeness, &generation.CreatedAt, &generation.PublishedAt,
		&generation.LastSelected, &generation.Error)
	if err == sql.ErrNoRows {
		return ViewGeneration{}, false, nil
	}
	if err != nil {
		return ViewGeneration{}, false, err
	}
	generation.LayerID = layerID.String
	generation.CheckoutID = checkoutID.String
	generation.ProvenanceCommitOID = provenance.String
	generation.BaseGenerationID = baseGeneration.Int64
	generation.State = ViewGenerationState(state)
	return generation, true, nil
}

// PublishViewGeneration is the building -> ready transition and the only write
// a generation ever receives after creation. The WHERE clause carries the
// expected state, so a generation that is already ready (or failed, or
// retiring) is immutable: the update matches nothing and reports
// ErrCatalogStaleGuard.
func (c *Catalog) PublishViewGeneration(ctx context.Context, generationID, publishedAt int64) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	return c.execGuarded(ctx, fmt.Sprintf("view generation %d is not building", generationID), `
UPDATE view_generations SET state = ?, published_at = ?
 WHERE generation_id = ? AND state = ?`,
		string(ViewGenerationReady), publishedAt, generationID, string(ViewGenerationBuilding))
}

// DeleteViewGeneration removes a generation nothing points at. SQLite's own
// foreign keys already refuse a delete under a route, a ref view, or another
// generation's base pointer (a non-deferred NO ACTION constraint is enforced
// as RESTRICT); this checks the same references — plus dedicated_graphs'
// deliberately key-free active pointer — first, so the caller gets one typed
// refusal instead of a driver constraint string.
func (c *Catalog) DeleteViewGeneration(ctx context.Context, generationID int64) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var referenced bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM checkout_routes WHERE commit_generation_id = ? OR dirty_generation_id = ?)
    OR EXISTS(SELECT 1 FROM ref_views WHERE active_generation_id = ?)
    OR EXISTS(SELECT 1 FROM view_generations WHERE base_generation_id = ?)
    OR EXISTS(SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?)`,
			generationID, generationID, generationID, generationID, generationID,
		).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM view_generations WHERE generation_id = ?`, generationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
		}
		return nil
	})
}

// --- view layers --------------------------------------------------------

// UpsertViewLayer writes one layer row.
func (c *Catalog) UpsertViewLayer(ctx context.Context, layer ViewLayer) error {
	if err := layer.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO view_layers (layer_id, kind, graph_id, checkout_id, target_ref, target_commit, target_tree)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(layer_id) DO UPDATE SET
  kind          = excluded.kind,
  graph_id      = excluded.graph_id,
  checkout_id   = excluded.checkout_id,
  target_ref    = excluded.target_ref,
  target_commit = excluded.target_commit,
  target_tree   = excluded.target_tree`,
		layer.LayerID, layer.Kind, layer.GraphID,
		catalogNullString(layer.CheckoutID), catalogNullString(layer.TargetRef),
		layer.TargetCommit, layer.TargetTree)
	return err
}

// GetViewLayer returns one layer.
func (c *Catalog) GetViewLayer(ctx context.Context, layerID string) (ViewLayer, bool, error) {
	layer := ViewLayer{LayerID: layerID}
	var checkoutID, targetRef sql.NullString
	err := c.store.db.QueryRowContext(ctx, `
SELECT kind, graph_id, checkout_id, target_ref, target_commit, target_tree
  FROM view_layers WHERE layer_id = ?`, layerID).Scan(
		&layer.Kind, &layer.GraphID, &checkoutID, &targetRef,
		&layer.TargetCommit, &layer.TargetTree)
	if err == sql.ErrNoRows {
		return ViewLayer{}, false, nil
	}
	if err != nil {
		return ViewLayer{}, false, err
	}
	layer.CheckoutID = checkoutID.String
	layer.TargetRef = targetRef.String
	return layer, true, nil
}

// --- checkout routes ----------------------------------------------------

// UpsertCheckoutRoute writes a checkout's route row, including its epoch.
// Repointing an existing route is FlipCheckoutRoute's job: this write does not
// compare-and-set, so it is for installing a route, not for moving one.
func (c *Catalog) UpsertCheckoutRoute(ctx context.Context, route CheckoutRoute) error {
	if err := route.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  graph_id             = excluded.graph_id,
  commit_generation_id = excluded.commit_generation_id,
  dirty_generation_id  = excluded.dirty_generation_id,
  route_epoch          = excluded.route_epoch,
  state                = excluded.state`,
		route.CheckoutID, route.GraphID, catalogNullInt(route.CommitGenerationID),
		catalogNullInt(route.DirtyGenerationID), route.RouteEpoch, string(route.State))
	return err
}

// GetCheckoutRoute returns one checkout's route.
func (c *Catalog) GetCheckoutRoute(ctx context.Context, checkoutID string) (CheckoutRoute, bool, error) {
	route := CheckoutRoute{CheckoutID: checkoutID}
	var (
		commitGen, dirtyGen sql.NullInt64
		state               string
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT graph_id, commit_generation_id, dirty_generation_id, route_epoch, state
  FROM checkout_routes WHERE checkout_id = ?`, checkoutID).Scan(
		&route.GraphID, &commitGen, &dirtyGen, &route.RouteEpoch, &state)
	if err == sql.ErrNoRows {
		return CheckoutRoute{}, false, nil
	}
	if err != nil {
		return CheckoutRoute{}, false, err
	}
	route.CommitGenerationID = commitGen.Int64
	route.DirtyGenerationID = dirtyGen.Int64
	route.State = RouteState(state)
	return route, true, nil
}

// DeleteCheckoutRoute withdraws a checkout's route. The route row is the one
// child of a checkout that does not cascade, so removing it is what unblocks
// DeleteCheckout.
func (c *Catalog) DeleteCheckoutRoute(ctx context.Context, checkoutID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("route for checkout %s", checkoutID),
		`DELETE FROM checkout_routes WHERE checkout_id = ?`, checkoutID)
}

// FlipCheckoutRoute repoints a route and bumps its epoch in one guarded
// statement. A flip carrying a stale epoch changes nothing and reports
// ErrCatalogStaleGuard, so two reconcilers cannot interleave halves of two
// different routes.
func (c *Catalog) FlipCheckoutRoute(ctx context.Context, req FlipCheckoutRouteRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, routeStates); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("route for checkout %s at epoch %d", req.CheckoutID, req.ExpectedRouteEpoch), `
UPDATE checkout_routes
   SET graph_id = ?, commit_generation_id = ?, dirty_generation_id = ?,
       route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`,
		req.GraphID, catalogNullInt(req.CommitGenerationID), catalogNullInt(req.DirtyGenerationID),
		string(req.State), req.CheckoutID, req.ExpectedRouteEpoch)
}

// --- ref views ----------------------------------------------------------

const refViewColumns = `graph_id, selector_kind, selector_value, desired_ref, desired_commit,
	desired_tree, active_generation_id, active_ref, active_commit, active_tree,
	enrichment_profile, desired_build_fingerprint, active_build_fingerprint, route_epoch,
	state, exact_view, last_resolved, last_selected, last_error`

// UpsertRefView writes one ref-view row. The UNIQUE selector key means a
// second row for the same (graph, selector, profile) is a constraint failure,
// not a duplicate view.
func (c *Catalog) UpsertRefView(ctx context.Context, view RefView) error {
	if err := view.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO ref_views (ref_view_id, `+refViewColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(ref_view_id) DO UPDATE SET
  graph_id                  = excluded.graph_id,
  selector_kind             = excluded.selector_kind,
  selector_value            = excluded.selector_value,
  desired_ref               = excluded.desired_ref,
  desired_commit            = excluded.desired_commit,
  desired_tree              = excluded.desired_tree,
  active_generation_id      = excluded.active_generation_id,
  active_ref                = excluded.active_ref,
  active_commit             = excluded.active_commit,
  active_tree               = excluded.active_tree,
  enrichment_profile        = excluded.enrichment_profile,
  desired_build_fingerprint = excluded.desired_build_fingerprint,
  active_build_fingerprint  = excluded.active_build_fingerprint,
  route_epoch               = excluded.route_epoch,
  state                     = excluded.state,
  exact_view                = excluded.exact_view,
  last_resolved             = excluded.last_resolved,
  last_selected             = excluded.last_selected,
  last_error                = excluded.last_error`,
		view.RefViewID, view.GraphID, view.SelectorKind, view.SelectorValue,
		view.DesiredRef, view.DesiredCommit, view.DesiredTree,
		catalogNullInt(view.ActiveGenerationID), catalogNullString(view.ActiveRef),
		catalogNullString(view.ActiveCommit), catalogNullString(view.ActiveTree),
		view.EnrichmentProfile, view.DesiredBuildFingerprint,
		catalogNullString(view.ActiveBuildFingerprint), view.RouteEpoch,
		string(view.State), catalogBoolInt(view.ExactView),
		view.LastResolved, view.LastSelected, view.LastError)
	return err
}

// scanRefView reads the refViewColumns projection in order.
func scanRefView(scan func(...any) error, view *RefView) error {
	var (
		activeGeneration                                       sql.NullInt64
		activeRef, activeCommit, activeTree, activeFingerprint sql.NullString
		state                                                  string
		exactView                                              int
	)
	if err := scan(
		&view.GraphID, &view.SelectorKind, &view.SelectorValue, &view.DesiredRef,
		&view.DesiredCommit, &view.DesiredTree, &activeGeneration, &activeRef,
		&activeCommit, &activeTree, &view.EnrichmentProfile,
		&view.DesiredBuildFingerprint, &activeFingerprint, &view.RouteEpoch,
		&state, &exactView, &view.LastResolved, &view.LastSelected, &view.LastError); err != nil {
		return err
	}
	view.ActiveGenerationID = activeGeneration.Int64
	view.ActiveRef = activeRef.String
	view.ActiveCommit = activeCommit.String
	view.ActiveTree = activeTree.String
	view.ActiveBuildFingerprint = activeFingerprint.String
	view.State = RefViewState(state)
	view.ExactView = exactView != 0
	return nil
}

// ListRefViews returns every named view rooted in one graph, ordered by view
// id. Retiring a graph has to find its views without knowing their ids, and
// the ref_views UNIQUE selector key makes graph_id the leading column of a
// usable index for the scan.
func (c *Catalog) ListRefViews(ctx context.Context, graphID string) ([]RefView, error) {
	rows, err := c.store.db.QueryContext(ctx,
		`SELECT ref_view_id, `+refViewColumns+` FROM ref_views WHERE graph_id = ? ORDER BY ref_view_id`, graphID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefView
	for rows.Next() {
		var view RefView
		err := scanRefView(func(dest ...any) error {
			return rows.Scan(append([]any{&view.RefViewID}, dest...)...)
		}, &view)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRefView removes a ref view. Its build attempts go with it through ON
// DELETE CASCADE; the generation it pointed at does not, because the pointer
// is the child side of the reference.
func (c *Catalog) DeleteRefView(ctx context.Context, refViewID string) error {
	if err := requireCatalogID("ref_view_id", refViewID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("ref view %s", refViewID),
		`DELETE FROM ref_views WHERE ref_view_id = ?`, refViewID)
}

// GetRefView returns one ref view.
func (c *Catalog) GetRefView(ctx context.Context, refViewID string) (RefView, bool, error) {
	view := RefView{RefViewID: refViewID}
	row := c.store.db.QueryRowContext(ctx,
		`SELECT `+refViewColumns+` FROM ref_views WHERE ref_view_id = ?`, refViewID)
	err := scanRefView(row.Scan, &view)
	if err == sql.ErrNoRows {
		return RefView{}, false, nil
	}
	if err != nil {
		return RefView{}, false, err
	}
	return view, true, nil
}

// --- ref view builds ----------------------------------------------------

const refViewBuildColumns = `ref_view_id, desired_ref, desired_commit, desired_tree,
	base_generation_id, enrichment_profile, build_fingerprint, generation_id,
	captured_route_epoch, state, build_token, created_at, last_progress, error`

// UpsertRefViewBuild writes one build attempt. While the row is in the
// building state the partial unique index coalesces requests: a second attempt
// for the same ref view, tree, base and fingerprint fails rather than racing
// the first to produce the same generation twice.
func (c *Catalog) UpsertRefViewBuild(ctx context.Context, build RefViewBuild) error {
	if err := build.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO ref_view_builds (build_id, `+refViewBuildColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(build_id) DO UPDATE SET
  ref_view_id          = excluded.ref_view_id,
  desired_ref          = excluded.desired_ref,
  desired_commit       = excluded.desired_commit,
  desired_tree         = excluded.desired_tree,
  base_generation_id   = excluded.base_generation_id,
  enrichment_profile   = excluded.enrichment_profile,
  build_fingerprint    = excluded.build_fingerprint,
  generation_id        = excluded.generation_id,
  captured_route_epoch = excluded.captured_route_epoch,
  state                = excluded.state,
  build_token          = excluded.build_token,
  created_at           = excluded.created_at,
  last_progress        = excluded.last_progress,
  error                = excluded.error`,
		build.BuildID, build.RefViewID, build.DesiredRef, build.DesiredCommit,
		build.DesiredTree, catalogNullInt(build.BaseGenerationID), build.EnrichmentProfile,
		build.BuildFingerprint, catalogNullInt(build.GenerationID),
		build.CapturedRouteEpoch, string(build.State), build.BuildToken,
		build.CreatedAt, build.LastProgress, build.Error)
	return err
}

// GetRefViewBuild returns one build attempt.
func (c *Catalog) GetRefViewBuild(ctx context.Context, buildID string) (RefViewBuild, bool, error) {
	build := RefViewBuild{BuildID: buildID}
	var (
		baseGeneration, generation sql.NullInt64
		state                      string
	)
	err := c.store.db.QueryRowContext(ctx,
		`SELECT `+refViewBuildColumns+` FROM ref_view_builds WHERE build_id = ?`, buildID).Scan(
		&build.RefViewID, &build.DesiredRef, &build.DesiredCommit, &build.DesiredTree,
		&baseGeneration, &build.EnrichmentProfile, &build.BuildFingerprint, &generation,
		&build.CapturedRouteEpoch, &state, &build.BuildToken, &build.CreatedAt,
		&build.LastProgress, &build.Error)
	if err == sql.ErrNoRows {
		return RefViewBuild{}, false, nil
	}
	if err != nil {
		return RefViewBuild{}, false, err
	}
	build.BaseGenerationID = baseGeneration.Int64
	build.GenerationID = generation.Int64
	build.State = ViewGenerationState(state)
	return build, true, nil
}

// --- cleanup journal ----------------------------------------------------

// UpsertCleanupEntry writes one deferred-deletion record. The journal has no
// foreign keys, so an entry outlives the rows it names.
func (c *Catalog) UpsertCleanupEntry(ctx context.Context, entry CleanupEntry) error {
	if err := entry.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO cleanup_journal
  (cleanup_id, opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cleanup_id) DO UPDATE SET
  opaque_target_ids = excluded.opaque_target_ids,
  reason            = excluded.reason,
  phase             = excluded.phase,
  grace_deadline    = excluded.grace_deadline,
  primary_epoch     = excluded.primary_epoch,
  last_progress     = excluded.last_progress,
  last_error        = excluded.last_error`,
		entry.CleanupID, entry.OpaqueTargetIDs, entry.Reason, string(entry.Phase),
		entry.GraceDeadline, entry.PrimaryEpoch, entry.LastProgress, entry.LastError)
	return err
}

// GetCleanupEntry returns one deferred-deletion record.
func (c *Catalog) GetCleanupEntry(ctx context.Context, cleanupID string) (CleanupEntry, bool, error) {
	entry := CleanupEntry{CleanupID: cleanupID}
	var phase string
	err := c.store.db.QueryRowContext(ctx, `
SELECT opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error
  FROM cleanup_journal WHERE cleanup_id = ?`, cleanupID).Scan(
		&entry.OpaqueTargetIDs, &entry.Reason, &phase, &entry.GraceDeadline,
		&entry.PrimaryEpoch, &entry.LastProgress, &entry.LastError)
	if err == sql.ErrNoRows {
		return CleanupEntry{}, false, nil
	}
	if err != nil {
		return CleanupEntry{}, false, err
	}
	entry.Phase = CleanupPhase(phase)
	return entry, true, nil
}

// ListCleanupEntries returns the whole journal, ordered by cleanup id. It is
// the recovery read: after a restart nobody knows which deletions were left
// half-done, so the resume pass enumerates the journal rather than addressing
// entries it would have to remember the ids of.
func (c *Catalog) ListCleanupEntries(ctx context.Context) ([]CleanupEntry, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT cleanup_id, opaque_target_ids, reason, phase, grace_deadline, primary_epoch,
       last_progress, last_error
  FROM cleanup_journal ORDER BY cleanup_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanupEntry
	for rows.Next() {
		var (
			entry CleanupEntry
			phase string
		)
		if err := rows.Scan(&entry.CleanupID, &entry.OpaqueTargetIDs, &entry.Reason, &phase,
			&entry.GraceDeadline, &entry.PrimaryEpoch, &entry.LastProgress, &entry.LastError); err != nil {
			return nil, err
		}
		entry.Phase = CleanupPhase(phase)
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteCleanupEntry removes one journal row. A cleanup deletes its own entry
// last, so the entry's absence is the record that the work finished.
func (c *Catalog) DeleteCleanupEntry(ctx context.Context, cleanupID string) error {
	if err := requireCatalogID("cleanup_id", cleanupID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("cleanup entry %s", cleanupID),
		`DELETE FROM cleanup_journal WHERE cleanup_id = ?`, cleanupID)
}
