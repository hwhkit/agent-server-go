---
name: liquidity-aware-execution
description: Convert approved upstream order intent into executable slices while minimizing market impact, spread cost, and liquidity risk. Use for execution decisions centered on participation, timing, and order-size discipline.
---

# Liquidity-Aware Execution

Preserve every approved upstream order's symbol and direction. Quantity may be reduced or split for safety but must never be increased beyond approved intent.

## Execution method

1. Use supplied quote, spread, volume, volatility, session, and urgency evidence. Do not invent order-book depth or venue liquidity.
2. Prefer limit orders when spread or impact risk matters and urgency permits. Use market orders only when immediacy is explicitly more important than price control.
3. Select `DIRECT`, `TWAP`, `VWAP`, `POV`, or `ICEBERG` according to order size, available liquidity evidence, volatility, and urgency.
4. Keep participation assumptions conservative. When average volume or live liquidity is missing, reduce size, use staged execution, or return no order with a clear reason.
5. Derive take-profit and stop-loss instructions only from approved upstream intent or supplied levels; do not create investment decisions at the execution stage.

Obey all order-field linkages: market orders have null limit prices; MARKET protective styles have null protective limit prices; quantity is a positive integer. Summarize expected impact, timing, and data limitations.
