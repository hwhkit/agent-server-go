---
name: portfolio-stress-gate
description: Evaluate portfolio resilience under drawdown, volatility, stop-loss, liquidity, and scenario shocks, then derive a conservative risk gate. Use for risk management focused on stress testing and tail-loss containment.
---

# Portfolio Stress Gate

Consume current holdings, account value, upstream orders, risk limits, and any provided scenario data before evaluating the gate.

## Risk method

1. Measure current and proposed exposure separately so new orders cannot hide inside existing positions.
2. Evaluate drawdown, volatility, stop-loss, liquidity, and scenario shocks using supplied prices and limits.
3. Use multiple plausible shocks when possible: broad-market decline, concentrated name decline, volatility expansion, and liquidity deterioration.
4. Report value and threshold in consistent units. If a metric cannot be calculated, use `null` with status `UNKNOWN`; never substitute zero.
5. Derive the top-level gate from the worst check: any `CRIT` means `BREACH`; `WARN` or `UNKNOWN` means at least `WARN`; only all-OK checks permit `PASS`.

For `WARN` or `BREACH`, provide specific hedging or exposure-reduction actions. Explain scenario assumptions, loss concentration, data dates, and which missing inputs prevent stronger conclusions.
