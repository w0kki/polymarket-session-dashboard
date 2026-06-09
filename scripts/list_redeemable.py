#!/usr/bin/env python3
"""
list_redeemable.py
==================

Lists all REDEEMABLE positions in the bot's Polymarket proxy wallet, with direct
Polymarket UI URLs for each. Use to click through and claim worthless settled
positions for wallet hygiene.

Note: redeeming $0 positions does NOT free up any USDC — these are losing bets
that resolved to zero. Cleaning them up is purely cosmetic and costs a small
amount of gas per redeem (~$0.01-0.05 on Polygon).

If you want the on-chain bulk-redeem instead (still costs gas per
condition), see the comments at the bottom for the contract details.

Usage:
    python3 scripts/list_redeemable.py
    python3 scripts/list_redeemable.py --json    # machine-readable output
    python3 scripts/list_redeemable.py --value   # only show positions with value > 0
"""

import argparse
import json
import sys
import urllib.request

PROXY_WALLET = "0x8aB08a84e5a64Bd46B62aD402a090971147350E3"
DATA_API = f"https://data-api.polymarket.com/positions?user={PROXY_WALLET}&sizeThreshold=1"


def fetch_positions():
    req = urllib.request.Request(
        DATA_API,
        headers={"User-Agent": "Mozilla/5.0 (polymarket-bot redeem tool)"},
    )
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read())


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--json", action="store_true", help="emit JSON instead of table")
    p.add_argument("--value", action="store_true", help="only show positions with currentValue > 0")
    args = p.parse_args()

    positions = fetch_positions()
    redeem = [pos for pos in positions if pos.get("redeemable")]
    if args.value:
        redeem = [pos for pos in redeem if pos.get("currentValue", 0) > 0]

    if args.json:
        print(json.dumps(redeem, indent=2, default=str))
        return

    if not redeem:
        print("No redeemable positions found.")
        return

    total_value = sum(p.get("currentValue", 0) for p in redeem)
    total_negrisk = sum(1 for p in redeem if p.get("negativeRisk"))

    print(f"Redeemable positions: {len(redeem)}  ({total_negrisk} negative-risk)")
    print(f"Total claimable USDC: ${total_value:.2f}")
    print()
    print(f"{'#':<3} {'Value':>8}  {'Outcome':<25} {'Market':<50}  URL")
    print("-" * 140)
    for i, pos in enumerate(redeem, 1):
        val = pos.get("currentValue", 0)
        title = (pos.get("title") or "")[:50]
        outcome = (pos.get("outcome") or "")[:25]
        slug = pos.get("slug") or ""
        url = f"https://polymarket.com/event/{slug}"
        neg = " [NR]" if pos.get("negativeRisk") else ""
        print(f"{i:<3} ${val:>6.2f}  {outcome:<25} {title:<50}  {url}{neg}")

    print()
    print(f"Polymarket UI: visit each URL and click 'Redeem' / 'Claim winnings'.")
    print(f"To skip ones worth $0 (no USDC to recover): python3 {sys.argv[0]} --value")


# ─────────────────────────────────────────────────────────────────────────────
# On-chain bulk redeem (NOT IMPLEMENTED — read this if you want to build it)
# ─────────────────────────────────────────────────────────────────────────────
#
# Polymarket positions are held by the PROXY WALLET (a Safe-style smart contract).
# To redeem, the proxy must call:
#
#   For regular markets:  ConditionalTokens.redeemPositions(
#                            collateralToken = USDC on Polygon,
#                            parentCollectionId = bytes32(0),
#                            conditionId = pos.conditionId,
#                            indexSets = [1, 2]  // both YES and NO for binary
#                         )
#     Contract: 0x4D97DCd97eC945f40cF65F87097ACe5EA0476045 (Polygon)
#
#   For negative-risk markets:  NegRiskAdapter.redeemPositions(
#                                  conditionId = pos.conditionId,
#                                  amounts = [pos.size for each outcome]
#                               )
#     Contract: 0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296 (Polygon)
#
# The EOA (POLY_PRIVATE_KEY) signs a Safe execTransaction on the proxy with the
# target=above_contract, data=encoded_calldata, operation=0 (CALL).
#
# Gas: roughly 80-150k per redeem. At 30 gwei × $0.5 MATIC ≈ $0.001-0.002 per tx.
# All 21 positions in this wallet's current state are worth $0, so the on-chain
# work recovers no USDC — purely wallet cleanup.
#
# To implement: use web3.py + the proxy's Safe ABI, OR build a small Go binary
# under bot/cmd/redeem/ that reuses live.go's wallet setup. Tag for follow-up
# only if real value sits unredeemed (i.e. the `--value` filter returns rows).


if __name__ == "__main__":
    main()
