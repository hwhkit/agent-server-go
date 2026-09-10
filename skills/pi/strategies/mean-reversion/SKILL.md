---
name: mean-reversion
description: Evaluate short-horizon mean-reversion opportunities in liquid US equities using daily price distance, RSI, ATR, and support/resistance. Use for stretched but non-breaking markets; do not fade a strongly confirmed breakout without reversal evidence.
---

# Mean Reversion

Use `mota_market_history` before applying this playbook. Require at least 30 valid daily bars and a usable ATR14 or equivalent price range.

## Decision method

For each symbol:

1. Compare the latest close with SMA20 and the recent 20-day range.
2. Look for a statistically meaningful stretch: a large move relative to ATR14, RSI14 near an extreme, and price near a well-observed range boundary.
3. Require evidence that the move is decelerating or rejecting the extreme. A new high/low with expanding volume is continuation evidence, not mean-reversion confirmation.
4. Avoid long reversion signals when `close < SMA20 < SMA50` remains strongly aligned; avoid short reversion signals under the symmetric bullish condition.
5. Use the mean or range midpoint as a potential target and place invalidation beyond the observed extreme with volatility allowance.

Emit `BUY` only for a confirmed downside stretch with stabilization; emit `SELL` only for a confirmed upside stretch with rejection. Otherwise omit the symbol.

Set `strategy_code` to `mean_reversion`. State the data date, size of the stretch, volatility context, confirmation, expected reversion anchor, and invalidation. This skill produces strategy signals, not executable orders.
