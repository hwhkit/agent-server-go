---
name: technical-factor-model
description: Produce systematic technical signals from a composite of trend, momentum, relative price behavior, volatility, volume, and liquidity factors. Use when no single chart pattern should dominate the technical assessment.
---

# Technical Factor Model

Use `mota_market_history` for every symbol before generating signals. Require comparable observation dates and sufficient daily bars.

## Factor framework

Evaluate each symbol across:

- Trend: close relative to SMA20/SMA50 and the alignment of those averages.
- Momentum: 20-day return, RSI14, and persistence rather than a single-day move.
- Breakout position: distance from the observed 20-day high/low.
- Volume/liquidity: average volume and latest volume ratio; penalize weak or unstable liquidity.
- Volatility: ATR14 relative to price and the width of the recent trading range.

Combine factors transparently. Do not invent a score for unavailable factors, and do not let one extreme indicator override conflicting evidence. Classify the regime and reduce confidence when trend, momentum, and volume disagree.

Derive `trend_state`, `bias`, `entry_zone`, targets, stop loss, key levels, and invalidation from returned bars. Use `NEUTRAL` with an empty `entry_zone` when the composite lacks directional agreement. Explain the data date and the factors that contributed positively, negatively, or were unavailable.
