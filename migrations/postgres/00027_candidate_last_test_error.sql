-- migrations/postgres/00027_candidate_last_test_error.sql
--
-- model_candidates.last_test_error stores the diagnostic of the most recent
-- probe that ran and failed; a passing probe clears it back to NULL. Probes
-- now also run asynchronously after a bulk import, so the reason a mapping
-- failed verification must survive the request that started it — before this
-- column the diagnostic only existed in the synchronous test response.

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN last_test_error TEXT NULL;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN last_test_error;
