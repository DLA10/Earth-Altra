import os
import sys
from dotenv import load_dotenv
from alpaca.trading.client import TradingClient

# EMERGENCY LIQUIDATE-ALL for one paper desk: cancels every open order and market-closes
# every position on that desk's account IN ONE Alpaca call (close_all_positions) — the
# instant, no-throttle alternative to the in-process reconciler's deliberate pacing.
#
#   python ml/liquidate.py RIDP     (also: RBT | CLAUDE | DIP | SNDK; default RBT)
#
# PAPER ONLY by construction: reads PAPER_<DESK>_KEY/SECRET and connects with paper=True.
# The live-money account's keys are never touched. Stop the backend first if you don't
# want the desk to re-enter afterwards.

DESKS = {"RBT", "RIDP", "CLAUDE", "DIP", "SNDK"}


def main():
    desk = (sys.argv[1] if len(sys.argv) > 1 else "RBT").upper()
    if desk not in DESKS:
        print(f"Unknown desk '{desk}'. Choose one of: {', '.join(sorted(DESKS))}")
        sys.exit(1)

    env_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "backend", ".env")
    load_dotenv(env_path)

    api_key = os.environ.get(f"PAPER_{desk}_KEY")
    api_secret = os.environ.get(f"PAPER_{desk}_SECRET")
    if not api_key or not api_secret:
        print(f"Error: PAPER_{desk}_KEY / PAPER_{desk}_SECRET not set in backend/.env")
        sys.exit(1)

    client = TradingClient(api_key, api_secret, paper=True)

    print(f"[{desk}] canceling all open orders and closing ALL positions (one call)...")
    try:
        responses = client.close_all_positions(cancel_orders=True)
        if not responses:
            print(f"[{desk}] account already flat.")
        for resp in responses:
            print(f"[{desk}] close submitted: {resp.symbol}")
        print(f"[{desk}] done — verify on the dashboard; fills complete within seconds in market hours.")
    except Exception as e:
        print(f"[{desk}] error liquidating positions: {e}")
        sys.exit(1)


if __name__ == "__main__":
    main()
