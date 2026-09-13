-- Lock the "free" tier so an unlicensed host can create nothing.
--
-- seedPricingPlans() only INSERTs when the row is missing, so an existing
-- database keeps the old permissive free plan (20 quizzes / 1 room / 20
-- players). That is deliberate — the seeder must not clobber limits an admin
-- tuned by hand — so this reconcile is a one-time manual step.
--
-- The gates compare `count >= limit`, so 0 blocks from the first attempt.
--
-- Run BEFORE setting LICENSE_ENFORCEMENT=true.

BEGIN;

UPDATE pricing_plans
SET name                   = 'Chưa kích hoạt',
    description            = 'Tài khoản chưa mua dịch vụ. Liên hệ quản trị viên để được cấp gói.',
    price_monthly_vnd      = 0,
    max_players_per_room   = 0,
    max_quizzes            = 0,
    max_templates          = 0,
    max_concurrent_rooms   = 0,
    max_questions_per_quiz = 0,
    allow_player_paced     = false,
    allow_custom_branding  = false,
    allow_export_logs      = false,
    allow_priority_support = false,
    allow_remove_watermark = false,
    is_active              = true,
    updated_at             = NOW()
WHERE id = 'free';

SELECT id, name, max_quizzes, max_concurrent_rooms, max_players_per_room
FROM pricing_plans ORDER BY sort_order;

COMMIT;
