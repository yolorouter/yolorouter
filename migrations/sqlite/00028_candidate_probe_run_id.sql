-- migrations/sqlite/00028_candidate_probe_run_id.sql
--
-- SQLite mirror of migrations/postgres/00028_candidate_probe_run_id.sql.
--
-- model_candidates.last_probe_run_id identifies the probe run that last wrote
-- this row's probe outcome. Every probe generates a fresh unique id and
-- commits it together with its verdict, guarded on the id it read before
-- probing — a compare-and-set that concurrent probes of the same mapping
-- (background queue, manual retests, other instances) cannot confuse the way
-- a timestamp can: ids never collide or compare "equal by coincidence", and a
-- writer can recognize its own already-applied write after a lost
-- acknowledgment by reading the id back. Empty string means no probe has
-- stamped the row yet.

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN last_probe_run_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN last_probe_run_id;
