---
name: diversification-risk-control
description: Detect single-name, sector, factor, correlation, and liquidity concentration across current and proposed exposures. Use for risk management focused on diversification quality and hidden exposure overlap.
---

# Diversification Risk Control

Evaluate the combined portfolio after applying proposed orders, not each position in isolation.

## Risk method

- Single-name concentration: compare each exposure with portfolio value and explicit limits.
- Sector/industry concentration: aggregate only when classification is available; otherwise mark the dimension `UNKNOWN`.
- Factor overlap: identify shared growth, value, momentum, duration, cyclicality, or volatility drivers from upstream evidence without manufacturing factor betas.
- Correlation: use supplied correlation or return-history evidence. Missing correlation is unknown, never assumed independent.
- Liquidity concentration: distinguish nominal diversification from positions that may become jointly difficult to exit.

Check both current holdings and incremental risk from upstream orders. A large number of symbols does not imply diversification when their economic drivers overlap.

Map findings to the strict risk contract. Use `WARN` with concrete diversification actions when important classification/correlation data is missing; use `BREACH` when an explicit concentration limit is violated.
