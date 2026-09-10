---
name: adaptive-order-routing
description: Choose order type, timing, time-in-force, execution instruction, and algorithm from approved intent and observable execution conditions. Use for execution decisions focused on reliable routing and fill-quality tradeoffs.
---

# Adaptive Order Routing

Transform approved upstream intent into contract-valid orders without changing symbol, direction, or maximum quantity.

## Routing method

- Urgency versus price certainty: choose market versus limit deliberately and explain the tradeoff.
- Time-in-force: use DAY for session-bounded intent and GTC only when persistence is justified.
- Execution instruction: use GFD, FAK, FOK, or BOC only when its fill behavior matches the stated objective.
- Algorithm: select DIRECT, TWAP, VWAP, POV, or ICEBERG from urgency, size, volatility, and available liquidity evidence.
- Failure handling: when quote, tick-size, session, or liquidity inputs are missing, avoid false precision and choose the most conservative contract-valid instruction or no order.

Do not invent unsupported venue fields, exchange microstructure, fill probability, or slippage estimates. Keep market consistent with the symbol, maintain price/style nullability rules, and preserve upstream protective levels where provided.

The execution summary must state routing rationale, timing, order-size treatment, expected tradeoff, and any missing data that constrained the choice.
