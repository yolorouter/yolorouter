--
-- SQLite mirror of migrations/postgres/00057_capability_enforcement_flags.sql.
--
-- The model-capability enforcement switches: six independent kill switches
-- plus one shared context-limit margin, seven system_settings rows that
-- share one version (CAS on save, same contract as the key-auto-recovery
-- pair seeded by 00048). Every switch ships OFF and the margin ships at 5%,
-- so an existing deployment changes nothing on upgrade. Column semantics
-- are shared with the postgres twin.

-- +goose Up
INSERT INTO system_settings (key, value) VALUES
    ('capability_context_limit_block', 'false'),
    ('capability_temperature_default', 'false'),
    ('capability_temperature_force', 'false'),
    ('capability_tools_strict', 'false'),
    ('capability_reasoning_strict', 'false'),
    ('capability_reasoning_egress_clear', 'false'),
    ('capability_context_margin_percent', '5');

-- +goose Down
DELETE FROM system_settings WHERE key IN (
    'capability_context_limit_block',
    'capability_temperature_default',
    'capability_temperature_force',
    'capability_tools_strict',
    'capability_reasoning_strict',
    'capability_reasoning_egress_clear',
    'capability_context_margin_percent');
