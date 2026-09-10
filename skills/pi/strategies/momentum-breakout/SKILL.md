---
name: momentum-breakout
description: Evaluate medium-term momentum and confirmed price/volume breakouts for US equities. Use when the strategist needs directional signals from daily OHLCV trends; do not use when fewer than 50 valid daily bars are available.
---

# Momentum Breakout

Use `mota_market_history` before applying this playbook. Work only from returned bars, indicators, quote timestamps, and explicit upstream evidence.

## Decision method

For each symbol:

1. Require at least 50 valid daily bars.
2. Treat `close > SMA20 > SMA50` as a positive trend and the inverse ordering as a negative trend. Otherwise classify the trend as mixed.
3. Confirm a bullish breakout only when price challenges or exceeds the prior 20-day high and volume is materially above its 20-day average. Apply the symmetric rule to bearish breakdowns.
4. Use RSI14 as confirmation, not as a standalone signal. Momentum is healthier in the middle-high range; extreme readings reduce confidence because reversal risk rises.
5. Derive invalidation and risk distance from recent support/resistance and ATR14. Never invent levels that are absent from the returned bars.

Emit a `BUY` or `SELL` strategist signal only when trend, breakout direction, and volume confirmation agree. If confirmation is incomplete, omit the symbol from `signals` and explain it in `rationale`.

Set `strategy_code` to `momentum_breakout`. Explain the observation date, trend alignment, breakout level, volume confirmation, invalidation, and major failure mode. This skill produces strategy signals, not executable orders.
