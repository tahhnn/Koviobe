-- Grandfather every account that is already using the product onto Pro
-- (lifetime, ends_at = NULL) before enforcement is switched on.
--
-- Without this, flipping LICENSE_ENFORCEMENT=true locks out existing hosts
-- mid-use — including the admin account, which also resolves entitlements
-- (internal/handler/admin_users.go:82).
--
-- Covers: every admin, plus any user who already owns a quiz, template or room.
-- Review the SELECT below before running the UPDATE.
--
-- Run AFTER 01_lock_free_plan.sql, BEFORE setting LICENSE_ENFORCEMENT=true.

BEGIN;

-- Preview: who is about to be grandfathered.
WITH active_users AS (
    SELECT u.id, u.email
    FROM users u
    LEFT JOIN roles r ON r.id = u.role_id
    WHERE u.deleted_at IS NULL
      AND (
            r.name = 'admin'
            OR EXISTS (SELECT 1 FROM quizzes q     WHERE q.host_id = u.id AND q.deleted_at IS NULL)
            OR EXISTS (SELECT 1 FROM templates t   WHERE t.host_id = u.id AND t.deleted_at IS NULL)
            OR EXISTS (SELECT 1 FROM rooms rm      WHERE rm.host_id = u.id AND rm.deleted_at IS NULL)
          )
)
SELECT id, email FROM active_users ORDER BY id;

-- Existing subscription rows -> pro, lifetime.
UPDATE subscriptions s
SET plan_id    = 'pro',
    status     = 'active',
    starts_at  = NOW(),
    ends_at    = NULL,
    updated_at = NOW()
FROM users u
LEFT JOIN roles r ON r.id = u.role_id
WHERE s.user_id = u.id
  AND s.deleted_at IS NULL
  AND u.deleted_at IS NULL
  AND (
        r.name = 'admin'
        OR EXISTS (SELECT 1 FROM quizzes q   WHERE q.host_id = u.id AND q.deleted_at IS NULL)
        OR EXISTS (SELECT 1 FROM templates t WHERE t.host_id = u.id AND t.deleted_at IS NULL)
        OR EXISTS (SELECT 1 FROM rooms rm    WHERE rm.host_id = u.id AND rm.deleted_at IS NULL)
      );

-- Users with no subscription row at all -> insert one on pro.
INSERT INTO subscriptions (user_id, plan_id, status, starts_at, ends_at, created_at, updated_at)
SELECT u.id, 'pro', 'active', NOW(), NULL, NOW(), NOW()
FROM users u
LEFT JOIN roles r ON r.id = u.role_id
WHERE u.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id AND s.deleted_at IS NULL)
  AND (
        r.name = 'admin'
        OR EXISTS (SELECT 1 FROM quizzes q   WHERE q.host_id = u.id AND q.deleted_at IS NULL)
        OR EXISTS (SELECT 1 FROM templates t WHERE t.host_id = u.id AND t.deleted_at IS NULL)
        OR EXISTS (SELECT 1 FROM rooms rm    WHERE rm.host_id = u.id AND rm.deleted_at IS NULL)
      );

SELECT s.user_id, u.email, s.plan_id, s.status, s.ends_at
FROM subscriptions s JOIN users u ON u.id = s.user_id
ORDER BY s.user_id;

COMMIT;
