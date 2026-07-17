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
        print("Error: Could not find credentials in backend/.env")
        sys.exit(1)
        
    client = TradingClient(api_key, api_secret, paper=True)
    
    print("Closing all open positions on Alpaca...")
    try:
        responses = client.close_all_positions(cancel_orders=True)
        for resp in responses:
            print(f"Closed position for {resp.symbol}")
        print("All ghost shares successfully liquidated!")
    except Exception as e:
        print(f"Error liquidating positions: {e}")

if __name__ == "__main__":
    main()
