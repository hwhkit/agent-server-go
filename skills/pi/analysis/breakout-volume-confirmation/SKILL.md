---
name: breakout-volume-confirmation
description: Detect actionable price breakouts and breakdowns using level penetration, close confirmation, relative volume, volatility, and retest behavior. Use for technical analysis centered on precise entry and invalidation levels.
---

# Breakout Volume Confirmation

Use `ir_market_history` before generating signals. Require enough daily bars to establish repeated support/resistance and a meaningful volume baseline.

## Confirmation rules

For each symbol:

1. Identify a level observed repeatedly in the returned bars; the 20-day high/low is context, not automatic proof.
2. Require a close through the level or a successful retest. Intraday penetration without close confirmation is weak evidence.
3. Confirm participation using latest volume versus the 20-day average. Low-volume breaks reduce confidence and may be false breakouts.
4. Check trend alignment and RSI14. Avoid chasing an exhausted move when price is excessively extended relative to ATR14.
5. Define entry around the breakout/retest zone, stop beyond the invalidation level with ATR allowance, and targets from observed range expansion or resistance/support.

Use `BREAKOUT` only for confirmed continuation and `OTHERS` or `SIDEWAYS` for unconfirmed tests. `LONG_BIAS` and `SHORT_BIAS` require symmetric evidence; otherwise use `NEUTRAL` with an empty entry zone.

Reasoning must state the observation date, trigger level, close confirmation, volume ratio, volatility context, targets, and exact invalidation condition. Never fabricate OHLCV or levels.
