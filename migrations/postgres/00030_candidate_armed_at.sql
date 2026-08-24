-- migrations/postgres/00030_candidate_armed_at.sql
--
-- model_candidates.armed_at pins WHEN the auto-enable promise was armed:
-- every write that arms the row (import, re-import requeue) sets it equal to
-- the same statement's updated_at. The probe commit's enable then requires
-- updated_at = armed_at still holding — and since every writer of this table
-- bumps updated_at, including binaries too old to know either armed column,
-- any write after arming breaks the equality and blocks the auto-enable.
-- That makes the row itself carry the revocation signal: an old binary's
-- explicit disable cannot clear auto_enable_on_pass, but it cannot help
-- bumping updated_at, and that alone is enough to keep the disable standing
-- through a mixed-version rollout.

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN armed_at TIMESTAMPTZ;
UPDATE model_candidates SET armed_at = updated_at WHERE auto_enable_on_pass;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN armed_at;
