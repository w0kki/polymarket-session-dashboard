#!/usr/bin/env python3
"""
reconcile_trades.py
====================

Reconcile the bot's `trades` table against actual on-chain activity for the
proxy wallet. Identifies trades where DB-recorded size_usdc / net_pnl was
inflated by partial fills (the bug fixed in commit bab477b) and emits a
SQL patch file.

Why this exists:
    The bot was recording the INTENDED order size in the DB even when the
    FAK order only partially filled. Real-world result: trades like
    Linda Fruhvirtova recorded as $85 / -$66 loss when only $4.73 of
    capital actually went through (-$3.60 real loss). The phantom losses
    accumulated to ~$200 in the bot's ledger vs the on-chain truth.

How it works:
    1. Pull complete activity history from Polymarket data-api
    2. Group BUYs and SELLs by conditionId+outcome
    3. Match each DB trade row to its on-chain BUY (and SELL if exited)
    4. Compute on-chain-truth values: actual_size_usdc, actual_shares,
       actual_net_pnl
    5. Print delta vs DB-recorded values
    6. Emit /tmp/reconcile_patch.sql for user to apply

Usage:
    python3 scripts/reconcile_trades.py                    # report only
    python3 scripts/reconcile_trades.py --threshold 0.5    # show all >50¢ diff
    python3 scripts/reconcile_trades.py --apply            # build SQL patch
"""
import argparse
import json
import sqlite3
import sys
import time
import urllib.request

PROXY = "0x8aB08a84e5a64Bd46B62aD402a090971147350E3"
ACTIVITY_API = "https://data-api.polymarket.com/activity?user={proxy}&limit={limit}&offset={offset}"
DB_REMOTE = "ubuntu@18.203.230.72:/home/ubuntu/app/trades.db"
DB_LOCAL = "/tmp/trades.db"
SSH_KEY = "~/.ssh/yfm-polly1-key.pem"


def fetch_activity():
    """Page through the activity endpoint until we've grabbed everything."""
    all_acts = []
    offset = 0
    limit = 500
    print(f"Fetching activity for {PROXY} ...", file=sys.stderr)
    while True:
        url = ACTIVITY_API.format(proxy=PROXY, limit=limit, offset=offset)
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=30) as r:
            page = json.loads(r.read())
        if not page:
            break
        all_acts.extend(page)
        print(f"  +{len(page)} records (total {len(all_acts)})", file=sys.stderr)
        if len(page) < limit:
            break
        offset += limit
        time.sleep(0.25)  # polite
    return all_acts


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--threshold", type=float, default=0.50,
                   help="report trades with |DB - on-chain net_pnl| > this (default 0.50)")
    p.add_argument("--apply", action="store_true",
                   help="write /tmp/reconcile_patch.sql for the SQL updates")
    p.add_argument("--no-fetch", action="store_true",
                   help="skip fresh activity fetch (use cached /tmp/activity.json)")
    args = p.parse_args()

    activity_path = "/tmp/activity.json"
    if args.no_fetch:
        with open(activity_path) as f:
            activity = json.load(f)
    else:
        activity = fetch_activity()
        with open(activity_path, "w") as f:
            json.dump(activity, f)

    # Group by conditionId+outcomeIndex for matching
    # asset id uniquely identifies a (condition, outcome) pair
    print(f"\nTotal activity records: {len(activity)}", file=sys.stderr)
    types = {}
    for a in activity:
        types[a.get("type")] = types.get(a.get("type"), 0) + 1
    print(f"By type: {types}", file=sys.stderr)

    # Build BUY and SELL aggregates per (conditionId, asset)
    # A single DB trade can correspond to MULTIPLE on-chain BUYs (the FAK retry
    # path fires multiple sub-orders), so we aggregate.
    buys = {}   # key=(condId, asset) -> [{timestamp, size, usdcSize}, ...]
    sells = {}  # same
    redeems = {}  # key=condId -> [{timestamp, usdcSize}]
    for a in activity:
        cid = a.get("conditionId")
        asset = a.get("asset", "")
        t = a.get("type")
        if t == "TRADE":
            side = a.get("side", "").upper()
            target = buys if side == "BUY" else sells if side == "SELL" else None
            if target is None:
                continue
            target.setdefault((cid, asset), []).append({
                "timestamp": a.get("timestamp", 0),
                "size": a.get("size", 0),
                "usdcSize": a.get("usdcSize", 0),
                "price": a.get("price", 0),
            })
        elif t == "REDEEM":
            redeems.setdefault(cid, []).append({
                "timestamp": a.get("timestamp", 0),
                "usdcSize": a.get("usdcSize", 0),
            })

    print(f"Unique (condition, asset) BUY positions: {len(buys)}", file=sys.stderr)
    print(f"Unique (condition, asset) SELL positions: {len(sells)}", file=sys.stderr)
    print(f"Unique condition REDEEMs: {len(redeems)}", file=sys.stderr)

    # Pull the trades from the local DB copy
    conn = sqlite3.connect(DB_LOCAL)
    conn.row_factory = sqlite3.Row
    rows = conn.execute("""
        SELECT condition_id, market, slug, side, sport, outcome,
               entry_price, exit_price, shares, size_usdc,
               pnl, net_pnl, buy_fee, sell_fee, total_fees,
               first_seen_at, updated_at
        FROM trades
        WHERE trade_type IN ('Risk Premia', 'Latency Arb')
          AND outcome != 'NA'
        ORDER BY first_seen_at
    """).fetchall()
    print(f"Settled live trades in DB: {len(rows)}", file=sys.stderr)

    # For each DB row, find the matching on-chain BUYs by conditionId
    # (we have to match across asset/outcome by side name -> can't do perfectly
    # without the outcome index, but the activity records include outcome strings)
    patches = []
    delta_total = 0.0
    matched = 0
    no_buy = 0
    partial_count = 0

    # Index activity by conditionId+outcome string for side matching
    buy_by_cidside = {}
    sell_by_cidside = {}
    for a in activity:
        if a.get("type") != "TRADE":
            continue
        cid = a.get("conditionId")
        outcome = a.get("outcome", "")
        key = (cid, outcome)
        target = buy_by_cidside if a.get("side", "").upper() == "BUY" else sell_by_cidside
        target.setdefault(key, []).append(a)

    # Track shares already sold per (cid, asset) so unredeemed shares = bought - sold
    sold_shares_by_key = {}
    for key, ss in sell_by_cidside.items():
        sold_shares_by_key[key] = sum(s.get("size", 0) for s in ss)

    # Track which conditions have been redeemed at all (helps detect "not yet
    # claimed" WIN positions).
    redeemed_cids = set(redeems.keys())

    for r in rows:
        cid = r["condition_id"]
        side_name = r["side"]
        key = (cid, side_name)
        bs = buy_by_cidside.get(key, [])
        ss = sell_by_cidside.get(key, [])
        if not bs:
            no_buy += 1
            continue
        matched += 1

        actual_shares_bought = sum(b.get("size", 0) for b in bs)
        actual_buy_usdc = sum(b.get("usdcSize", 0) for b in bs)
        actual_avg_price = (actual_buy_usdc / actual_shares_bought) if actual_shares_bought > 0 else 0

        actual_sell_usdc = sum(s.get("usdcSize", 0) for s in ss)
        actual_sold_shares = sum(s.get("size", 0) for s in ss)
        actual_redeem_usdc = sum(z.get("usdcSize", 0) for z in redeems.get(cid, []))

        # Account for shares that haven't been redeemed yet:
        #  - shares_remaining = bought - sold
        #  - if outcome==WIN, those shares will redeem at $1 each → add to value
        #  - if outcome==LOSS, those shares are worth $0 → no value to add
        # This catches "Resolved ✓ but not yet redeemed" WIN positions, otherwise
        # the script reports a phantom loss equal to the full buy.
        shares_remaining = max(0, actual_shares_bought - actual_sold_shares)
        pending_value = 0
        outcome = (r["outcome"] or "").upper()
        if outcome == "WIN" and shares_remaining > 0 and cid not in redeemed_cids:
            pending_value = shares_remaining * 1.0  # binary $1 payout
        # For LOSSes, shares_remaining sit at $0 — no addition

        actual_net = actual_sell_usdc + actual_redeem_usdc + pending_value - actual_buy_usdc

        db_net = r["net_pnl"] or 0
        db_size = r["size_usdc"] or 0
        delta = actual_net - db_net

        if abs(actual_buy_usdc - db_size) > 0.5 or abs(delta) > args.threshold:
            partial_count += 1
            delta_total += delta
            patches.append({
                "condition_id": cid,
                "market": (r["market"] or "")[:50],
                "outcome": r["outcome"],
                "db_size": db_size,
                "actual_size": actual_buy_usdc,
                "db_shares": r["shares"] or 0,
                "actual_shares": actual_shares_bought,
                "db_net_pnl": db_net,
                "actual_net_pnl": actual_net,
                "delta": delta,
                "entry_price": actual_avg_price or r["entry_price"],
                "exit_price": r["exit_price"],
                "pending": pending_value,
            })

    print(f"\nMatched: {matched}, no-buy: {no_buy}")
    print(f"Partial-fill / discrepant trades: {partial_count}")
    print(f"Net P&L correction (positive = phantom loss removed): ${delta_total:+.2f}\n")

    if patches:
        # Sort by |delta| descending
        patches.sort(key=lambda p: -abs(p["delta"]))
        print(f"{'#':<3} {'Δ':>8}  {'DB net':>8}  {'Real net':>9}  {'DB size':>8}  {'Real size':>10}  Market / Side")
        print("-" * 130)
        for i, pt in enumerate(patches[:30], 1):
            print(f"{i:<3} {pt['delta']:>+8.2f}  {pt['db_net_pnl']:>+8.2f}  {pt['actual_net_pnl']:>+9.2f}  "
                  f"{pt['db_size']:>8.2f}  {pt['actual_size']:>10.4f}  {pt['market']} / {pt['outcome']}")
        if len(patches) > 30:
            print(f"... and {len(patches) - 30} more (rerun with --threshold 0 to see all)")

    if args.apply and patches:
        sql_path = "/tmp/reconcile_patch.sql"
        with open(sql_path, "w") as f:
            f.write("-- Auto-generated by scripts/reconcile_trades.py\n")
            f.write(f"-- Reconciliation date: {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime())}\n")
            f.write(f"-- Trades patched: {len(patches)}\n")
            f.write(f"-- Net P&L correction: ${delta_total:+.2f}\n")
            f.write("BEGIN TRANSACTION;\n\n")
            for pt in patches:
                # Use actual buy USDC as size_usdc, actual shares as shares.
                # net_pnl = actual on-chain delta. pnl (gross) ≈ net_pnl + fees (we leave fees as-is).
                f.write(
                    f"UPDATE trades SET "
                    f"size_usdc = {pt['actual_size']:.4f}, "
                    f"shares = {pt['actual_shares']:.6f}, "
                    f"entry_price = {pt['entry_price']:.4f}, "
                    f"net_pnl = {pt['actual_net_pnl']:.4f}, "
                    f"pnl = {pt['actual_net_pnl'] + (0 or 0):.4f}  -- gross approx = net (fee fields untouched)\n"
                    f"WHERE condition_id = '{pt['condition_id']}';\n"
                )
            f.write("\n-- Inspect the changes, then run:\n-- COMMIT;\n")
            f.write("-- Or to abort:\n-- ROLLBACK;\n")
        print(f"\nSQL patch written to {sql_path}")
        print(f"Review it, then apply with: ssh ... 'sqlite3 /home/ubuntu/app/trades.db < /tmp/reconcile_patch.sql'")


if __name__ == "__main__":
    main()
