import os
import sys
from dotenv import load_dotenv
from alpaca.trading.client import TradingClient

def main():
    env_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "backend", ".env")
    load_dotenv(env_path)
    
    api_key = os.environ.get("PAPER_RBT_KEY")
    api_secret = os.environ.get("PAPER_RBT_SECRET")
    
    if not api_key or not api_secret:
        print("Error: Could not find PAPER_RBT_KEY or PAPER_RBT_SECRET in backend/.env")
        sys.exit(1)
        
    client = TradingClient(api_key, api_secret, paper=True)
    
    positions = client.get_all_positions()
    if not positions:
        print("No open positions found in Alpaca.")
    else:
        for p in positions:
            print(f"Symbol: {p.symbol}, Qty: {p.qty}, Entry Price: {p.avg_entry_price}, Current Price: {p.current_price}, PnL: {p.unrealized_pl}")
            
    from alpaca.trading.requests import GetOrdersRequest
    from alpaca.trading.enums import QueryOrderStatus
    
    req = GetOrdersRequest(status=QueryOrderStatus.ALL, limit=10)
    orders = client.get_orders(filter=req)
    print("\nLast 10 Orders:")
    for o in orders:
        print(f"Symbol: {o.symbol}, Side: {o.side}, Qty: {o.qty}, Status: {o.status}, Type: {o.order_type}, Submitted: {o.submitted_at}")

if __name__ == "__main__":
    main()
