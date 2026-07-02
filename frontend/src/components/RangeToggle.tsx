import { CHART_RANGES, type ChartView } from "../types";

// RangeToggle is the shared chart control: live intraday timeframes (1m/5m/10m) on the
// left, historical ranges (1W/1M/6M/1Y) on the right. Intraday views stream over the
// WebSocket; ranges load static REST history. Reuses the existing .tf-toggle styles.
const INTRADAY: ChartView[] = ["1m", "5m", "10m"];

export function RangeToggle({ view, onChange }: { view: ChartView; onChange: (v: ChartView) => void }) {
  return (
    <div className="range-toggle">
      <div className="tf-toggle">
        {INTRADAY.map((v) => (
          <button key={v} className={`tf-btn ${view === v ? "on" : ""}`} onClick={() => onChange(v)}>
            {v}
          </button>
        ))}
      </div>
      <div className="tf-toggle">
        {CHART_RANGES.map((v) => (
          <button key={v} className={`tf-btn ${view === v ? "on" : ""}`} onClick={() => onChange(v)}>
            {v}
          </button>
        ))}
      </div>
    </div>
  );
}
