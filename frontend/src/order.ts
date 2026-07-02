// Persisted custom ordering for symbol lists. Single-user local app → localStorage.

export function loadOrder(key: string): string[] {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as string[]) : [];
  } catch {
    return [];
  }
}

export function saveOrder(key: string, symbols: string[]): void {
  try {
    localStorage.setItem(key, JSON.stringify(symbols));
  } catch {
    /* ignore quota/serialization errors */
  }
}

// applyOrder arranges `symbols` by `saved` (saved entries first, in saved order); any
// symbols not in `saved` (newly added) are appended in their original order.
export function applyOrder(symbols: string[], saved: string[]): string[] {
  const present = new Set(symbols);
  const out = saved.filter((s) => present.has(s));
  const seen = new Set(out);
  for (const s of symbols) if (!seen.has(s)) out.push(s);
  return out;
}

// move returns a new array with the item at `from` moved to index `to`.
export function move<T>(arr: T[], from: number, to: number): T[] {
  if (from === to || from < 0 || to < 0 || from >= arr.length || to >= arr.length) return arr;
  const copy = arr.slice();
  const [item] = copy.splice(from, 1);
  copy.splice(to, 0, item);
  return copy;
}
