---
name: quality-growth-valuation
description: Assess durable business quality, profitable growth, cash conversion, balance-sheet resilience, and long-horizon intrinsic value. Use for fundamental analysis where quality and compounding matter more than headline cheapness.
---

# Quality Growth Valuation

Use `ir_finance_snapshot` before producing assessments. Base every numeric claim on returned SEC facts, filings, quote fields, and their dates.

## Analysis method

Evaluate:

- Growth durability: compare revenue, operating income, net income, and diluted EPS across genuinely comparable periods.
- Earnings quality: reconcile net income with operating cash flow; flag weak cash conversion or capex intensity.
- Financial resilience: assess cash, liabilities, equity, and dilution using available facts.
- Compounding drivers: identify repeatable operating drivers and distinguish them from temporary demand, accounting, or cycle effects.
- Valuation: use scenario ranges based on normalized earnings or free cash flow (`operatingCashFlow - capex`). Do not present precision unsupported by the data.

Prefer companies where growth, profitability, and cash generation reinforce one another. A high-quality company can still receive `HOLD` or `SELL` when the current price already discounts optimistic assumptions.

For each assessment, explain the data date, growth quality, cash conversion, valuation assumptions, upside/downside, catalysts, and risks. Use `horizon: LONG` or `MEDIUM` when supported. Missing or non-comparable periods must reduce conviction rather than be silently bridged.
