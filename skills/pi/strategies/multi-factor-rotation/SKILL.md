---
name: multi-factor-rotation
description: Rank a multi-symbol US equity universe using comparable momentum, quality, growth, valuation context, and liquidity evidence. Use when the strategist must select or weight candidates across at least three symbols; avoid cross-sectional conclusions when comparable data is missing.
---

# Multi-Factor Rotation

Use `ir_market_history` for every candidate and `ir_finance_snapshot` when fundamental facts are required. Apply this playbook only when at least three symbols have comparable observations from the same approximate date.

## Decision method

Build a relative ranking rather than isolated absolute scores:

- Momentum: 20-day return, position versus SMA20/SMA50, and breakout persistence.
- Quality: operating profitability and operating cash generation when SEC facts are available.
- Growth: comparable recent revenue and earnings direction; do not compare mismatched fiscal periods without disclosure.
- Valuation context: market value and available valuation fields, treated as context rather than a timing signal.
- Liquidity/risk: average volume, ATR-normalized volatility, drawdown behavior, and missing-data penalties.

Do not manufacture a composite score from unavailable factors. Reduce conviction when fundamental periods are stale, units differ, or only price factors are present. Prefer a small number of clearly differentiated signals over forcing every candidate into the output.

Use `BUY` for higher-ranked candidates with positive absolute confirmation and `SELL` only when both relative rank and absolute evidence are negative. A low relative rank alone is not a sell signal.

Set `strategy_code` to `multi_factor_rotation`. In `rationale`, state the comparison date, universe, factor availability, ranking drivers, position-sizing logic, and reasons excluded symbols were not selected. This skill produces strategy signals, not executable orders.
