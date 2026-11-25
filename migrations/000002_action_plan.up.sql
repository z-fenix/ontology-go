-- Write-back runs (Spec §7.5) are reserved before their workflow starts; the plan holds the
-- validated edits and rendered side effects so the workflow can commit without re-validating.
ALTER TABLE edits.action_run ADD COLUMN IF NOT EXISTS plan JSONB;
