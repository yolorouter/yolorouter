-- migrations/sqlite/00029_candidate_auto_enable_on_pass.sql
--
-- SQLite mirror of migrations/postgres/00029_candidate_auto_enable_on_pass.sql.
--
-- model_candidates.auto_enable_on_pass records the IMPORT FLOW'S standing
-- promise on a row: bulk import stores mappings disabled+untested and arms
-- this flag, and the background probe queue may enable the mapping on a pass
-- only while the flag is still set — checked inside the commit statement
-- itself, so the decision reflects the row at write time. An administrator's
-- explicit disable clears the flag, revoking the promise whether the probe is
-- still queued or already in flight (a value-based status check cannot see a
-- disable that changes no stored value). A decisive verdict consumes the
-- flag; a re-import requeue arms it again.

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN auto_enable_on_pass BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN auto_enable_on_pass;
