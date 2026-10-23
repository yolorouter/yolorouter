--
-- Two pieces of derived state on model_candidates, persisted beside the raw
-- columns they are derived from because stored zeros cannot carry them:
--
--   prices_declared  whether the row's primary price slots were filled at
--                    write time. An unpriced row and an explicitly free one
--                    both hold zero in the price columns, so the write paths
--                    record the distinction here rather than re-reading it
--                    out of numbers that cannot answer.
--   probe_owed       whether the background queue still owes this row a
--                    probe. auto_enable_on_pass used to double as that
--                    durable key, but an unpriced import row is probed while
--                    never auto-enabled — armed alone stopped saying "owed a
--                    probe" — so the probe debt moves to its own column and
--                    the flag goes back to meaning only the enable promise.
--
-- The backfill judges legacy rows the way the pre-column read path did, so
-- no row flips state across the upgrade:
--
--   prices_declared — a model whose stored output declaration is
--   image-exclusive (image without text) or video-exclusive (["video"])
--   counts as declared outright; every other row counts as declared when any
--   of its four token prices is non-zero and unpriced when all four are.
--   Audio-mode rows are judged by their token slots too: those slots were
--   the row's stored price state before the column, and the character price
--   was never one of the four prices the unpriced reading looked at.
--
--   probe_owed — exactly the rows the startup recovery used to collect:
--   armed, untested and never stamped.
--
-- The backfill also disarms rows that must not auto-enable on a pass: a row
-- the backfill judged unpriced, and any row whose model serves image or
-- video output (those bill through tier tables a bulk import never carries,
-- so an auto-enable would put an unpriced mapping into rotation). probe_owed
-- is computed BEFORE the disarming statement, from the original armed state —
-- a disarmed row is still owed the probe its queue entry promised.
-- Column semantics are shared with the sqlite twin.
--

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN prices_declared BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE model_candidates ADD COLUMN probe_owed BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE model_candidates
SET probe_owed = (auto_enable_on_pass IS TRUE
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
WHERE auto_enable_on_pass IS TRUE
  AND (prices_declared IS NOT TRUE
       OR EXISTS (SELECT 1 FROM models
                  WHERE models.id = model_candidates.model_id
                    AND (output_modalities LIKE '%"image"%'
                         OR output_modalities LIKE '%"video"%')));

-- +goose Down
-- Dropping the columns is all the downgrade does: it does NOT promise that a
-- later re-up reproduces the state the first Up created. The backfills judge
-- from the pre-column columns, and the first Up has already rewritten one of
-- them: rows it disarmed (unpriced, or on an image/video-output model) now
-- carry auto_enable_on_pass = FALSE, so a re-up's probe_owed backfill marks
-- them owed = FALSE even though their probe debt was never settled by any
-- queue. The disarm decision and the probe-debt verdict are therefore not
-- idempotent across a down/up cycle — a known, accepted trade-off, not
-- something the Down section tries to preserve.
ALTER TABLE model_candidates DROP COLUMN IF EXISTS probe_owed;
ALTER TABLE model_candidates DROP COLUMN IF EXISTS prices_declared;
