import json
import time
import os

data = []
current_time = int(time.time()) - 500 * 60

for i in range(500):
    data.append({
        "time": current_time + i * 60,
        "open": 100.0 + i*0.01,
        "high": 100.5 + i*0.01,
        "low": 99.5 + i*0.01,
        "close": 100.2 + i*0.01,
        "volume": 1000
    })

with open('recent_bars.json', 'w') as f:
    json.dump(data, f)
    
print("Created recent_bars.json")
