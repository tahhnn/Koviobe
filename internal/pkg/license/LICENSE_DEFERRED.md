# License / Pro gating — DEFERRED

**Status:** Deferred (2026-07)  
**Kill switch:** `EnforcementEnabled = false` in `license.go`

## Intent

Commercial licensing (Free vs Pro) will be developed later. Until then:

- Do **not** block hosts on plan limits or Pro-only features.
- Keep plan catalog, subscriptions, and admin assign APIs for future work.
- Re-enable product gates by setting `EnforcementEnabled = true`.

## Planned Free vs Pro (when re-enabled)

| Limit / feature        | Free | Pro  |
|------------------------|------|------|
| Players / room         | 20   | 200  |
| Questions / quiz       | 30   | 100  |
| Concurrent rooms       | 1    | 10   |
| Quizzes                | 20   | ∞    |
| Templates              | 10   | ∞    |
| Solo (player-paced)    | no   | yes  |
| Custom branding / logo | no   | yes  |
| Export detailed logs   | no   | yes  |
| Remove watermark       | no   | yes  |

## Gates that are currently open (via `openEntitlements`)

Backend handlers still *call* `GetEntitlements`, but with enforcement off they receive open limits:

- `CreateQuiz` — quiz count, question count, player-paced theme, branding
- `UpdateQuiz` — question count
- `CreateRoom` — concurrent rooms, question count, player-paced
- `JoinRoom` — max players per room
- `CreateTemplate` — template count
- `ExportRoomLogs` — `allow_export_logs`

Frontend: Solo launch no longer checks Pro before creating a room.

## How to turn enforcement back on

1. Set `EnforcementEnabled = true` in `internal/pkg/license/license.go`.
2. Restart API gateway.
3. Verify Free host is blocked on Solo / >30 questions / >20 players as expected.
4. Re-enable any frontend Pro checks (quiz editor Solo) if desired for UX.
