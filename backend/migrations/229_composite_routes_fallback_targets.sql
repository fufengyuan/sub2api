-- Add ordered fallback provider targets to composite model routes for
-- multi-platform failover (primary target first, fallbacks in order).
ALTER TABLE composite_model_routes
    ADD COLUMN IF NOT EXISTS fallback_targets jsonb;
