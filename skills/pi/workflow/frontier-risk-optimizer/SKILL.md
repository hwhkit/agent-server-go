---
name: frontier-risk-optimizer
description: Build risk-efficient portfolios by balancing expected return evidence, volatility, correlation, diversification, and active constraints. Use for portfolio management focused on efficient-frontier reasoning and risk-adjusted allocation.
---

# Frontier Risk Optimizer

Use upstream research and risk outputs to compare candidate portfolios on expected reward versus risk. Do not claim a numerical efficient frontier unless expected returns and a defensible covariance or scenario matrix are provided.

## Optimization method

- Expected return: derive ordinal or bounded estimates from upstream evidence; disclose proxies and dates.
- Risk: use provided volatility, drawdown, stress, concentration, and correlation evidence. Treat missing correlation as unknown, not zero.
- Diversification: penalize redundant exposures and reward independent return drivers only when supported by data.
- Constraints: enforce cash, position, market, turnover, and risk limits before ranking candidate allocations.
- Robustness: prefer allocations that remain acceptable across reasonable return/risk perturbations rather than a fragile point estimate.

If numerical inputs are incomplete, perform qualitative frontier reasoning and reduce conviction. Explain which assets improve the risk-return tradeoff and which are excluded because of uncertainty or redundant exposure.

Respect the portfolio manager's `isFinal` dual contract. This skill may recommend allocation or final orders but must not fabricate optimization statistics, covariance values, or automatic execution.
