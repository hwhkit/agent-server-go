---
name: constraint-aware-allocation
description: Convert research, account state, holdings, cash, and explicit portfolio constraints into practical target allocations and rebalance decisions. Use for portfolio management centered on implementable allocation changes rather than unconstrained optimization.
---

# Constraint-Aware Allocation

Consume the complete upstream DAG, current account, positions, pending orders, symbol scope, and explicit limits before proposing changes.

## Allocation method

1. Separate existing holdings from new candidates and identify duplicated exposure across upstream recommendations.
2. Translate signal direction and conviction into target weights only after applying cash, position-size, concentration, drawdown, and market-scope constraints actually present in the task.
3. Prefer small, explainable weight changes over unnecessary turnover. Include current weight when available and explain each delta.
4. Resolve upstream disagreement explicitly. Risk-manager output overrides a higher-return proposal when the constraint is binding.
5. When evidence is insufficient, use `NO_ACTION`, empty orders, and a non-empty target allocation that preserves the current configuration.

For `isFinal=false`, produce the process decision contract with `decision_type`, target allocation or orders, and a concrete rationale. For `isFinal=true`, produce only the final timestamp/orders/report structure. Never mix both branches.

Do not invent cash, prices, correlations, or limits. State which constraints were applied and what residual risk is handed to risk management.
