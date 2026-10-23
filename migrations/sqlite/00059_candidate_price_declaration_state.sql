--
-- SQLite mirror of migrations/postgres/00059_candidate_price_declaration_state.sql
-- (the payload statements are the same — both backends evaluate the boolean
-- expressions and the correlated EXISTS subqueries identically here). See the
-- postgres twin's header note for the full column contract.
--

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN prices_declared BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE model_candidates ADD COLUMN probe_owed BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE model_candidates
SET probe_owed = (auto_enable_on_pass
                  AND verification_status = 0
                  AND last_tested_at IS NULL);

UPDATE model_candidates
SET prices_declared = CASE
    WHEN EXISTS (SELECT 1 FROM models
                 WHERE models.id = model_candidates.model_id
                   AND output_modalities LIKE '%"image"%'
                   AND output_modalities NOT LIKE '%"text"%') THEN TRUE
    WHEN EXISTS (SELECT 1 FROM models
                 WHERE models.id = model_candidates.model_id
                   AND output_modalities = '["video"]') THEN TRUE
    ELSE NOT (COALESCE(input_price, 0) = 0
              AND COALESCE(output_price, 0) = 0
              AND COALESCE(cache_write_price, 0) = 0
              AND COALESCE(cache_read_price, 0) = 0)
END;

UPDATE model_candidates
SET auto_enable_on_pass = FALSE
WHERE auto_enable_on_pass
  AND (prices_declared IS NOT 1
       OR EXISTS (SELECT 1 FROM models
                  WHERE models.id = model_candidates.model_id
                    AND (output_modalities LIKE '%"image"%'
                         OR output_modalities LIKE '%"video"%')));

-- +goose Down
-- Same caveat as the postgres twin: dropping the columns does NOT make the
-- derived state idempotent across a down/up cycle. A re-up re-judges
-- probe_owed from auto_enable_on_pass, which the first Up set FALSE on the
-- rows it disarmed — so rows disarmed in the first round come back
-- owed = FALSE with their probe debt still unsettled. Neither the disarm nor
-- the owed verdict survives a down/up cycle; a known, accepted trade-off.
ALTER TABLE model_candidates DROP COLUMN probe_owed;
ALTER TABLE model_candidates DROP COLUMN prices_declared;
