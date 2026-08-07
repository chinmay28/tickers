#!/usr/bin/env python
import pprint
import requests
import yfinance as yf
from datetime import datetime


def main():
    tickers = ["VTI", "GLD", "P", "ORCL", "STRC", "IBIT", "BTC-USD"]
    ticker_objs = {t: yf.Ticker(t) for t in tickers}

    quotes = {}

    for t in tickers:
        try:
            data = ticker_objs[t].history(period="1d", interval="1m")
            quotes[t] = f"{data['Close'].iloc[-1]:.2f}" if not data.empty else "N/A"
        except Exception:
            quotes[t] = "N/A"

    quotes["timestamp"] = datetime.now().strftime("%m/%d %H:%M:%S")

    pprint.pprint(quotes)

    base_url = "http://100.84.70.60:9999/api/entries"
    key = "minion-quotes"

    try:
        # ✅ Try PUT first
        response = requests.put(f"{base_url}/{key}", json={"value": quotes, "category": "minion"})
        response.raise_for_status()
        print("PUT succeeded")
    except Exception as exc:
        print("PUT failed, trying POST:", exc)

        try:
            response = requests.post(
                base_url,
                json={"key": key, "value": quotes}
            )
            response.raise_for_status()
            print("POST succeeded")
        except Exception as exc2:
            print("POST also failed:", exc2)
            return

    pprint.pprint(response.status_code)

    try:
        pprint.pprint(response.json())
    except Exception:
        print("No JSON response:", response.text)


if __name__ == "__main__":
    main()
