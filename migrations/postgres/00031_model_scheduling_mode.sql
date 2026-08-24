-- migrations/postgres/00031_model_scheduling_mode.sql
--
-- models.scheduling_mode decides how the gateway orders a model's candidate
-- chain for each request: 'failover' walks the admin's sort_order with the
-- first candidate taking all traffic until it fails; 'balanced' spreads
-- caller API keys across providers via per-key sticky bindings. Existing
-- rows keep today's behaviour.

-- +goose Up
ALTER TABLE models ADD COLUMN scheduling_mode TEXT NOT NULL DEFAULT 'failover';

-- +goose Down
ALTER TABLE models DROP COLUMN scheduling_mode;
