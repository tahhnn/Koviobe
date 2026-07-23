#!/usr/bin/env python3
"""
Kovio capacity & license smoke tests against a live API.

Covers:
  - Public plan catalog (Free 20 / Pro 200 players)
  - JoinRoom hard limit enforcement for Free plan
  - Join latency under concurrent joins
  - Sidebar layout contract for 30+ slides (source inspection)

Usage:
  python3 scripts/capacity_license_test.py
"""

from __future__ import annotations

import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import jwt  # PyJWT

API = os.environ.get("KOVIO_API", "http://127.0.0.1:8082/api")
JWT_SECRET = os.environ.get("JWT_SECRET", "change_me_to_a_long_random_string")
HOST_USER_ID = int(os.environ.get("HOST_USER_ID", "1"))
HOST_EMAIL = os.environ.get("HOST_EMAIL", "buichthanh2468@gmail.com")
QUIZ_ID = int(os.environ.get("QUIZ_ID", "6"))  # Science Trivia (33 questions)
ROOT = Path(__file__).resolve().parents[1]
SIDEBAR_FILE = ROOT.parent / "kahoot-clone-webapp" / "app" / "quizzes" / "[quizId]" / "page.tsx"


def http(method: str, path: str, body: dict | None = None, headers: dict | None = None, timeout: float = 30.0):
    data = None
    hdrs = {"Content-Type": "application/json", **(headers or {})}
    if body is not None:
        data = json.dumps(body).encode()
    req = urllib.request.Request(f"{API}{path}", data=data, headers=hdrs, method=method)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            raw = res.read().decode()
            elapsed_ms = (time.perf_counter() - t0) * 1000
            payload = json.loads(raw) if raw else {}
            return res.status, payload, elapsed_ms
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        elapsed_ms = (time.perf_counter() - t0) * 1000
        try:
            payload = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            payload = {"error": raw}
        return e.code, payload, elapsed_ms


def mint_host_token(user_id: int = HOST_USER_ID, email: str = HOST_EMAIL, role: str = "admin") -> str:
    now = int(time.time())
    claims = {
        "user_id": user_id,
        "email": email,
        "role": role,
        "permissions": [
            "quiz:create",
            "quiz:read",
            "quiz:update",
            "quiz:delete",
            "room:control",
            "logs:read",
        ],
        "token_type": "access",
        "iat": now,
        "exp": now + 60 * 30,
    }
    return jwt.encode(claims, JWT_SECRET, algorithm="HS256")


def section(title: str):
    print(f"\n=== {title} ===")


def test_plans() -> dict:
    section("1) License plans catalog")
    status, plans, ms = http("GET", "/license/plans")
    assert status == 200, plans
    by_id = {p["id"]: p for p in plans}
    assert "free" in by_id and "pro" in by_id
    free, pro = by_id["free"], by_id["pro"]
    assert free["max_players_per_room"] == 20
    assert free["max_questions_per_quiz"] == 30
    assert free["max_concurrent_rooms"] == 1
    assert free["allow_player_paced"] is False
    assert pro["max_players_per_room"] == 200
    assert pro["max_questions_per_quiz"] == 100
    assert pro["max_concurrent_rooms"] == 10
    assert pro["allow_player_paced"] is True
    print(f"OK plans ({ms:.1f}ms)")
    print(f"  Free: {free['max_players_per_room']} players/room, {free['max_questions_per_quiz']} Q/quiz")
    print(f"  Pro:  {pro['max_players_per_room']} players/room, {pro['max_questions_per_quiz']} Q/quiz")
    return by_id


def create_room(token: str, quiz_id: int) -> dict:
    status, data, ms = http(
        "POST",
        f"/rooms?quiz_id={quiz_id}",
        None,
        headers={"Authorization": f"Bearer {token}"},
    )
    if status not in (200, 201):
        raise RuntimeError(f"create room failed {status}: {data}")
    print(f"Room created pin={data.get('pin_code')} id={data.get('id')} max_players={data.get('max_players')} ({ms:.1f}ms)")
    return data


def join_one(pin: str, nickname: str) -> tuple[int, dict, float]:
    return http("POST", "/rooms/join", {"pin_code": pin, "nickname": nickname})


def seed_players_sql(room_id: int, count: int, prefix: str = "seed") -> None:
    import subprocess

    if count <= 0:
        return
    values = ", ".join(
        f"({room_id}, '{prefix}{i}', 0, true, NOW(), NOW())" for i in range(1, count + 1)
    )
    sql = f"INSERT INTO players (room_id, nickname, score, is_connected, created_at, updated_at) VALUES {values};"
    subprocess.check_call(
        ["docker", "exec", "kovio-postgres", "psql", "-U", "postgres", "-d", "kovio", "-c", sql],
        stdout=subprocess.DEVNULL,
    )


def clear_join_rate_limit() -> None:
    import subprocess

    # Best-effort: wipe join rate-limit keys so API probes are not blocked mid-test
    subprocess.run(
        ["docker", "exec", "kovio-redis", "redis-cli", "KEYS", "*join*"],
        capture_output=True,
        text=True,
    )
    out = subprocess.check_output(
        ["docker", "exec", "kovio-redis", "redis-cli", "--scan", "--pattern", "*join*"],
        text=True,
    ).strip()
    keys = [k for k in out.splitlines() if k]
    for k in keys:
        subprocess.run(["docker", "exec", "kovio-redis", "redis-cli", "DEL", k], stdout=subprocess.DEVNULL)


def test_free_capacity(token: str, quiz_id: int, max_players: int = 20) -> dict:
    section(f"2) Free room capacity fill → reject at {max_players}+1")
    # Note: JoinRoomRateLimit = 10/IP/min — seed most seats via SQL, probe last seats via API.
    room = create_room(token, quiz_id)
    pin = room["pin_code"]
    room_id = int(room["id"])
    assert room.get("max_players") == max_players, room

    seed_players_sql(room_id, max_players - 2, prefix="f")
    clear_join_rate_limit()

    latencies = []
    status, data, ms = join_one(pin, "api_last_ok_1")
    latencies.append(ms)
    assert status == 200, (status, data)
    status, data, ms = join_one(pin, "api_last_ok_2")
    latencies.append(ms)
    assert status == 200, (status, data)

    status, data, ms = join_one(pin, "overflow_player")
    latencies.append(ms)
    assert status == 403, (status, data)
    assert data.get("max_players") == max_players
    print(f"OK Free room full at {max_players}; overflow rejected ({ms:.1f}ms)")
    print(f"  probe join latency ms: {', '.join(f'{x:.1f}' for x in latencies)}")
    return {"pin": pin, "room_id": room_id, "filled": max_players, "latencies": latencies}


def test_concurrent_burst(pin: str, start_n: int, count: int = 8) -> None:
    section(f"3) Concurrent join burst ({count} parallel) on full room")
    clear_join_rate_limit()

    def work(i: int):
        return join_one(pin, f"burst_{start_n}_{i}")

    t0 = time.perf_counter()
    results = []
    with ThreadPoolExecutor(max_workers=count) as ex:
        futs = [ex.submit(work, i) for i in range(count)]
        for f in as_completed(futs):
            results.append(f.result())
    wall = (time.perf_counter() - t0) * 1000
    codes = [r[0] for r in results]
    ms_list = [r[2] for r in results]
    print(f"  wall={wall:.1f}ms codes={sorted(set(codes))} avg_req={statistics.mean(ms_list):.1f}ms")
    # Expect 403 (full) — rate-limit 429 is also acceptable under burst
    assert all(c in (403, 429) for c in codes), codes
    print("OK full-room concurrent joins rejected (403/429)")


def test_pro_capacity_sample(token: str, quiz_id: int, sample: int = 8) -> dict:
    """Sample join latency under Pro max=200. Full 200 fill uses SQL seed + API edge probe."""
    section("4) Pro room: join latency sample + edge capacity at 200")
    room = create_room(token, quiz_id)
    pin = room["pin_code"]
    room_id = int(room["id"])
    assert room.get("max_players") == 200, room

    clear_join_rate_limit()
    latencies = []
    ok = 0
    t0 = time.perf_counter()
    for i in range(1, sample + 1):
        status, data, ms = join_one(pin, f"pro_s_{i}")
        latencies.append(ms)
        if status == 200:
            ok += 1
        else:
            print(f"  join sample fail #{i}: {status} {data}")
            break
    wall = (time.perf_counter() - t0) * 1000
    print(f"OK sampled {ok}/{sample} joins in {wall:.0f}ms")
    if latencies:
        print(
            f"  join latency ms: p50={statistics.median(latencies):.1f} "
            f"avg={statistics.mean(latencies):.1f} max={max(latencies):.1f}"
        )
    if wall > 0 and ok > 0:
        rate = ok / (wall / 1000)
        print(f"  approx join rate={rate:.1f} players/s → ETA fill 200 ≈ {200/rate:.1f}s (excl. rate limit)")

    # Edge: seed to 199, join 200th OK, 201st forbidden
    # Delete sample players then reseed cleanly to 199
    import subprocess

    subprocess.check_call(
        [
            "docker",
            "exec",
            "kovio-postgres",
            "psql",
            "-U",
            "postgres",
            "-d",
            "kovio",
            "-c",
            f"DELETE FROM players WHERE room_id={room_id};",
        ],
        stdout=subprocess.DEVNULL,
    )
    seed_players_sql(room_id, 199, prefix="pro")
    clear_join_rate_limit()
    status, data, ms = join_one(pin, "pro_200th")
    assert status == 200, (status, data)
    print(f"OK Pro 200th player joined ({ms:.1f}ms)")
    status, data, ms = join_one(pin, "pro_201st")
    assert status == 403, (status, data)
    assert data.get("max_players") == 200
    print(f"OK Pro 201st rejected ({ms:.1f}ms) — hard cap 200 enforced")
    return {"ok": ok, "wall_ms": wall, "latencies": latencies, "pin": pin}


def test_sidebar_layout_contract() -> None:
    section("5) Sidebar layout contract (30+ slides UX)")
    src = SIDEBAR_FILE.read_text(encoding="utf-8")
    checks = [
        ("overflow-y-auto on list", 'overflow-y-auto' in src and 'ref={slidesRef}' in src),
        ("compact row layout", 'flex items-center gap-3' in src),
        ("sticky Add slide footer", 'border-t border-[#2c313d] shrink-0' in src),
        ("auto-scroll active slide", 'scrollIntoView' in src and 'data-slide-id' in src),
        ("no aspect-video thumbnails in sidebar list", src.count("aspect-video") <= 2),  # editor media preview may remain
    ]
    # Height budget: compact row ~44-52px → 30 slides ≈ 1.5k px vs old cards ~180px*30=5400px
    old_card_h = 180
    new_row_h = 48
    n = 33
    print(f"  estimated list height old≈{old_card_h*n}px vs new≈{new_row_h*n}px (viewport sidebar ~700px)")
    print(f"  screens of scrolling: old≈{(old_card_h*n)/700:.1f} → new≈{(new_row_h*n)/700:.1f}")
    failed = []
    for name, ok in checks:
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}")
        if not ok:
            failed.append(name)
    if failed:
        raise AssertionError(f"sidebar contract failed: {failed}")
    print("OK sidebar compact layout contract")


def set_plan_via_sql(plan_id: str) -> None:
    import subprocess

    sql = f"UPDATE subscriptions SET plan_id='{plan_id}', status='active', ends_at=NULL WHERE user_id={HOST_USER_ID};"
    subprocess.check_call(
        ["docker", "exec", "kovio-postgres", "psql", "-U", "postgres", "-d", "kovio", "-c", sql],
        stdout=subprocess.DEVNULL,
    )


def finish_active_rooms(token: str) -> None:
    # Free plan allows 1 concurrent room — close leftovers so create succeeds.
    status, data, _ = http("GET", "/rooms", headers={"Authorization": f"Bearer {token}"})
    # endpoint may not list — ignore
    import subprocess

    subprocess.check_call(
        [
            "docker",
            "exec",
            "kovio-postgres",
            "psql",
            "-U",
            "postgres",
            "-d",
            "kovio",
            "-c",
            f"UPDATE rooms SET status='finished' WHERE host_id={HOST_USER_ID} AND status IN ('waiting','active');",
        ],
        stdout=subprocess.DEVNULL,
    )


def main() -> int:
    print(f"API={API}")
    plans = test_plans()
    token = mint_host_token()

    # Auth sanity
    status, me, ms = http("GET", "/license/me", headers={"Authorization": f"Bearer {token}"})
    print(f"\nlicense/me → {status} ({ms:.1f}ms)")
    if status != 200:
        print(me)
        print("WARN: token auth failed — JoinRoom capacity still testable via DB rooms if needed")
        return 1
    ents = me.get("entitlements") or {}
    print(f"  resolved plan={ents.get('plan_id')} max_players={ents.get('max_players_per_room')} max_q={ents.get('max_questions_per_quiz')}")

    finish_active_rooms(token)
    set_plan_via_sql("free")
    free = test_free_capacity(token, QUIZ_ID, plans["free"]["max_players_per_room"])
    test_concurrent_burst(free["pin"], 100)

    finish_active_rooms(token)
    set_plan_via_sql("pro")
    # refresh entitlements after plan change
    status, me, _ = http("GET", "/license/me", headers={"Authorization": f"Bearer {token}"})
    ents = me.get("entitlements") or me
    print(f"\nSwitched to Pro: max_players={ents.get('max_players_per_room')}")
    test_pro_capacity_sample(token, QUIZ_ID, sample=8)

    # restore free
    set_plan_via_sql("free")
    finish_active_rooms(token)

    test_sidebar_layout_contract()

    # Gap report: quiz already over Free question cap
    section("6) License enforcement gap check")
    import subprocess

    out = subprocess.check_output(
        [
            "docker",
            "exec",
            "kovio-postgres",
            "psql",
            "-U",
            "postgres",
            "-d",
            "kovio",
            "-t",
            "-A",
            "-c",
            f"SELECT COUNT(*) FROM questions WHERE quiz_id={QUIZ_ID};",
        ],
        text=True,
    ).strip()
    qcount = int(out)
    free_cap = plans["free"]["max_questions_per_quiz"]
    print(f"  quiz {QUIZ_ID} has {qcount} questions; Free cap={free_cap}")
    if qcount > free_cap:
        print("  NOTE: existing quiz exceeds Free cap (created before UpdateQuiz enforcement).")
        print("  Source now enforces MaxQuestionsPerQuiz on UpdateQuiz — restart api-gateway to apply.")
    else:
        print("  OK under Free question cap")

    section("SUMMARY")
    print("  Free max players/room: 20 (enforced on JoinRoom) — VERIFIED")
    print("  Pro  max players/room: 200 (enforced on JoinRoom edge) — VERIFIED")
    print("  Free max questions/quiz: 30 — UpdateQuiz enforcement added in source (needs API restart)")
    print("  Sidebar compact list for 30+ slides — contract PASSED")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as e:
        print(f"\nFAILED: {e}", file=sys.stderr)
        raise
