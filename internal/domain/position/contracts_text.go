package position

// 预置岗位输出契约常量。正文逐字取自 Rust 侧
// src/platform/executor/mod.rs 的六个 *_CONTRACT 常量,不得改写:
// 这些文本就是模型面对的协议,任何措辞变动都会改变输出行为。

// FundamentalAnalystContract 是 FUNDAMENTAL_ANALYST_CONTRACT 的等价物。
// 基本面分析师输出契约(prompt 注入;键名与评测的岗位契约严格一致)。
const FundamentalAnalystContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。结构如下:

{
  "payload_result": {
    "assessments": [
      {
        "symbol": "AAPL.US",
        "rating": "BUY",
        "fair_value": 245.0,
        "reasoning": "……",
        "score": 8.2,
        "conviction": 0.76,
        "horizon": "MEDIUM",
        "upside_pct": 12.5,
        "valuation": {"pe": 28.5, "pb": 6.1},
        "key_drivers": ["服务收入增长"],
        "key_risks": ["估值压缩"]
      }
    ],
    "evidenceRefs": [
      { "sourceType": "TASK_INPUT", "sourceId": "task" }
    ],
    "summary": "……"
  }
}

硬性要求:
- assessments 必填;正常完成时至少一条,合法失败路径允许空数组并在 summary 写明具体原因。每项必填 symbol、rating、fair_value(数字)、reasoning(非空)。
- rating 只能是五档之一: STRONG_BUY / BUY / HOLD / SELL / STRONG_SELL(不要用 NEUTRAL 等非枚举值)。
- 可选增强字段: score 为 0-10,conviction 为 0-1,horizon 只能 INTRADAY/SHORT/MEDIUM/LONG;upside_pct 为数字;valuation 为对象;key_drivers/key_risks 为字符串数组。
- reasoning 必须说明估值、增长/盈利质量、催化剂和主要风险；没有可靠数据时明确说明缺口，不编造财务数值。
- summary 必填非空。
- 不要输出契约之外的任何字段;评估的标的必须来自任务输入给出的标的,不得改写代码。`

// TechnicalAnalystContract 是 TECHNICAL_ANALYST_CONTRACT 的等价物。
// 技术分析师输出契约:payload_result.signals[](键名与 technical-analyst 岗位契约严格一致)。
const TechnicalAnalystContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。结构如下:

{
  "payload_result": {
    "signals": [
      {
        "symbol": "AAPL.US",
        "trend_state": "UPTREND",
        "bias": "LONG_BIAS",
        "entry_zone": [240.0, 242.0],
        "reasoning": "……",
        "interval": "1d",
        "momentum_score": 0.62,
        "confidence": 0.71,
        "stop_loss": 235.0,
        "targets": [250.0, 260.0],
        "key_levels": {"support": [235.0], "resistance": [250.0]},
        "pattern": "突破后回踩",
        "indicators": {"rsi_14": 61.4, "macd": "bullish"},
        "volatility_regime": "NORMAL",
        "relative_strength": "vs benchmark +6.4%",
        "invalidation": "收盘跌破 235.0 后信号失效"
      }
    ],
    "summary": "……",
    "evidenceRefs": [
      { "sourceType": "TASK_INPUT", "sourceId": "task" }
    ]
  }
}

硬性要求:
- signals 必填;正常完成时至少一条,缺少可靠行情或模型失败时允许空数组并在 summary 写明具体原因。每项必填 symbol、bias、entry_zone、reasoning(非空)。
- trend_state 只能是枚举之一: UPTREND / DOWNTREND / SIDEWAYS / BREAKOUT / PULLBACK / MEAN_REVERSION / OTHERS(不要用中文或"上升趋势"等非枚举值)。
- bias 只能是枚举之一: LONG_BIAS / SHORT_BIAS / NEUTRAL(不要用 BULLISH / BEARISH)。
- entry_zone 只能是空数组或恰好两个数字 [下沿,上沿]，不能用描述文本代替。
- momentum_score 必须在 -1 到 1；confidence 必须在 0 到 1（不要输出 72、85 这类百分数）。
- targets/support/resistance 必须是数字数组；key_levels 只能含 support/resistance；indicators 可保存 RSI/MACD/ATR/均线/量比等结构化读数。
- reasoning 应结合趋势、量价、动量、关键位与失效条件；没有行情数据时使用 NEUTRAL + 空 entry_zone 并明确数据缺口。
- 不要输出契约之外的任何字段;评估的标的必须来自任务输入给出的标的,不得改写代码。`

// StrategistContract 是 STRATEGIST_CONTRACT 的等价物。
// 策略分析师输出契约:payload_result.signals[] + rationale(键名与 strategist 岗位契约严格一致)。
const StrategistContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。结构如下:

{
  "payload_result": {
    "signals": [
      {
        "symbol": "AAPL.US",
        "direction": "BUY",
        "target_weight_pct": 12.5,
        "quantity": 100,
        "price": 245.0,
        "horizon": "MEDIUM",
        "reasoning": "……"
      }
    ],
    "strategy_code": "momentum_rotation_v3",
    "market_regime": "ROTATION",
    "rationale": "……",
    "views": [{"lens":"STYLE_FACTOR","subject":"动量","stance":"OVERWEIGHT"}],
    "backtest": {"annualized_return_pct": 24.3, "sharpe": 1.85, "max_drawdown_pct": 14.2},
    "key_risks": ["风格快速反转"],
    "evidenceRefs": [
      { "sourceType": "TASK_INPUT", "sourceId": "task" }
    ]
  }
}

硬性要求:
- signals 必填;正常完成时至少一条。没有有效标的/可验证方向或模型失败时允许空数组,但 rationale 必须说明原因,不得编造方向。
- rationale 必填非空。
- 可选字段: target_weight_pct/quantity/price(数字)、horizon(INTRADAY/SHORT/MEDIUM/LONG)、strategy_code、market_regime(RISK_ON/RISK_OFF/SIDEWAYS/ROTATION/OTHERS)、views、backtest、key_risks。
- views 每项只能含 lens/subject/stance；lens 枚举 STYLE_FACTOR/MACRO/TECHNICAL/EVENT_DRIVEN/SEASONALITY/CROWDING/SECTOR/ASSET_CLASS/OTHERS，stance 枚举 OVERWEIGHT/NEUTRAL/UNDERWEIGHT。
- 本岗只输出策略信号，不输出 orders；rationale 应说明市场状态、信号合成、仓位方向、历史验证与风险。
- 不要输出契约之外的任何字段;评估的标的必须来自任务输入给出的标的,不得改写代码。`

// RiskManagerContract 是 RISK_MANAGER_CONTRACT 的等价物。
// 风控经理输出契约:payload_result.gate + checks[](键名与 risk-manager 岗位契约严格一致)。
const RiskManagerContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。结构如下:

{
  "payload_result": {
    "gate": "PASS",
    "checks": [
      { "dimension": "SINGLE_NAME", "status": "OK", "value": 6.2, "threshold": 10.0, "reasoning": "……", "detail": "……" }
    ],
    "interpretation": "整体风险解读",
    "hedging_suggestions": ["当 gate 为 WARN/BREACH 时给出可执行建议"],
    "evidenceRefs": [
      { "sourceType": "TASK_INPUT", "sourceId": "task" }
    ]
  }
}

硬性要求:
- gate 必填,只能是枚举之一: PASS / WARN / BREACH(不要用 APPROVED / BLOCKED)。
- checks 必填且至少一条;每项必填 dimension、status、reasoning(非空)。
- dimension 只能是枚举之一: SINGLE_NAME / SECTOR_EXPOSURE / FACTOR_EXPOSURE / VAR / VOLATILITY / STYLE_FACTOR / DRAWDOWN / STRESS_TEST / LIQUIDITY / CONCENTRATION / STOP_LOSS / TAKE_PROFIT / OTHERS。
- status 只能是枚举之一: OK / WARN / CRIT / UNKNOWN(不要用 PASS / FAIL)。
- value/threshold 可为数字或 null；缺数据/无适用阈值时填 null，并把 status 设为 UNKNOWN，禁止编造数值。
- 可选字段: detail、hedging_suggestions(字符串数组)、exposures(对象)、gating_actions(字符串数组)、interpretation。
- gate 必须与 checks 一致:任一 checks.status 为 WARN/CRIT/UNKNOWN 时,gate 不应为 PASS(应为 WARN 或 BREACH)。
- gate 为 WARN 或 BREACH 时 hedging_suggestions 必填且至少一条；全 OK 才能 PASS，任一 CRIT 必须 BREACH。
- 失败路径:当某些风险指标在当前数据条件下无法计算、或任务无法完整完成时,把对应 check 的 status 置为 UNKNOWN(或 WARN)并在 reasoning 说明,
  同时 gate 必须为 WARN(不是 PASS),并给出 hedging_suggestions 建议。
- 不要输出契约之外的任何字段。`

// ExecutionTraderContract 是 EXECUTION_TRADER_CONTRACT 的等价物。
// 执行交易员输出契约:payload_result.orders[](键名与 execution-trader 岗位契约严格一致)。
const ExecutionTraderContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。结构如下:

{
  "payload_result": {
    "orders": [
      {
        "market": "US",
        "symbol": "AAPL.US",
        "direction": "BUY",
        "order_type": "LIMIT",
        "limit_price": 245.0,
        "quantity": 100,
        "take_profit_trigger_price": 260.0,
        "take_profit_order_style": "LIMIT",
        "take_profit_limit_price": 259.5,
        "stop_loss_trigger_price": 235.0,
        "stop_loss_order_style": "MARKET",
        "stop_loss_limit_price": null,
        "time_in_force": "DAY",
        "execution_instruction": "GFD",
        "algo_type": "VWAP",
        "algo_params": {"participation_rate_pct": 12},
        "reasoning": "……"
      }
    ],
    "timestamp": "2026-08-20T13:35:00Z",
    "execution_summary": "订单数量、名义金额、滑点、成本与流动性摘要"
  }
}

硬性要求:
- orders 必填;正常完成时按上游订单意图输出，合法失败路径允许空数组。每项必填以下 13 个字段: market、symbol、direction、order_type、limit_price、quantity、take_profit_trigger_price、take_profit_order_style、take_profit_limit_price、stop_loss_trigger_price、stop_loss_order_style、stop_loss_limit_price、reasoning(非空)。
- market 只能是 US / HK / CN;direction 只能是 BUY / SELL;order_type 只能是 MARKET / LIMIT;quantity 必须是正整数。
- MARKET 单 limit_price 必须为 null；LIMIT 单 limit_price 必须为正数。take_profit/stop_loss style 为 MARKET 时对应 limit_price 必须为 null。
- 可选字段: time_in_force(DAY / GTC)、execution_instruction(GFD / FAK / FOK / BOC)、algo_type(DIRECT / TWAP / VWAP / POV / ICEBERG)、algo_params。
- 必须保留任务输入 upstream.nodes[].output 中上游给出的全部订单意图:对每个标的至少产出一条 orders(quantity 为正数、方向与上游一致),
  不得漏掉任意一个上游标的;数量允许小于上游原始数量(拆单/分批),但不得放大、不得改方向。
- 若任务简报要求「从 symbolPool 精选 N 只」,应从上游订单中选取(N 只),而不要引入上游没有的标的;确需精选时仍需覆盖尽可能多的上游标的。
- 不要输出契约之外的任何字段;标的必须来自任务输入给出的标的,不得改写代码。`

// PortfolioManagerContract 是 PORTFOLIO_MANAGER_CONTRACT 的等价物。
// 组合经理输出契约:payload_result.decision_type + orders[] + target_allocation[]
// (键名与 portfolio-manager 岗位契约严格一致)。
const PortfolioManagerContract = `只输出一个 JSON 对象,不要 markdown 代码块、不要调用工具、不要输出 DSML 或任何解释文字。按 runtimeContext.isFinal 二选一输出,不要混合两套结构。

【职能一:isFinal 缺省或为 false(团队过程中的 PM,配置决策)】结构如下:
{
  "payload_result": {
    "decision_type": "STANDALONE",
    "orders": [
      { "symbol": "AAPL.US", "market": "US", "direction": "BUY", "order_type": "MARKET", "quantity": 100, "reasoning": "……" }
    ],
    "target_allocation": [
      { "symbol": "AAPL.US", "target_weight_pct": 12.5, "current_weight_pct": 9.0, "stance": "OVERWEIGHT", "reasoning": "……" }
    ],
    "rationale": "……",
    "evidenceRefs": [ { "sourceType": "TASK_INPUT", "sourceId": "task" } ]
  }
}
硬性要求(职能一):
- decision_type 必填,枚举: STANDALONE / MULTI_SOURCE_CONSENSUS / CONFLICT_RESOLUTION / RISK_OVERRIDE / REBALANCE / NO_ACTION / OTHERS。
- orders 每项必填 symbol、market(US / HK / CN)、direction(BUY / SELL)、order_type(MARKET / LIMIT)、quantity(正整数)、reasoning(非空)。
- target_allocation 每项必填 symbol、target_weight_pct(数字)、reasoning(非空);可选 stance(OVERWEIGHT / NEUTRAL / UNDERWEIGHT)、current_weight_pct。
- orders 与 target_allocation 至少一项非空;NO_ACTION 也须以非空 target_allocation 表达「维持现状」，不可只给空 orders。
- rationale 应综合完整 upstream 祖先图中的分歧/共识、账户与持仓、风险限制、权重变化和交给风控的剩余风险。
- 失败路径:当任务简报明确表示「上游研究结论不足以支撑决策」「无法完成」「要求保守缺省」等时,
  必须把 decision_type 置为 NO_ACTION(不要继续给出决策类型),orders 给空数组,
  target_allocation 用非空数组表达「维持现状」,rationale 说明原因。
- rationale 必填非空;可选 constraints_applied、risk_handoff。

【职能二:isFinal = true(final 岗位,完整订单信号)】结构如下:
{
  "payload_result": {
    "timestamp": "2026-08-14T00:00:00Z",
    "orders": [
      { "time": "09:30:00", "ticker": "META.US", "name": "Meta Platforms", "market": "US", "direction": "BUY", "quantity": 100, "order_type": "LIMIT", "limit_price": 245.0, "take_profit_trigger_price": 260.0, "take_profit_order_style": "LIMIT", "take_profit_limit_price": 259.5, "stop_loss_trigger_price": 235.0, "stop_loss_order_style": "MARKET", "stop_loss_limit_price": null, "time_in_force": "DAY", "execution_instruction": "GFD", "reasoning": "……" }
    ],
    "report_markdown": "……",
    "evidenceRefs": [ { "sourceType": "TASK_INPUT", "sourceId": "task" } ]
  }
}
硬性要求(职能二):
- timestamp、orders、report_markdown 必填。
- 订单每项必填: time、ticker、name、market、direction、quantity、order_type、limit_price、take_profit_trigger_price、take_profit_order_style、take_profit_limit_price、stop_loss_trigger_price、stop_loss_order_style、stop_loss_limit_price、time_in_force、execution_instruction、reasoning。
- 落单硬约束: quantity 正整数;market 必须与 ticker 后缀一致;order_type=LIMIT 时 limit_price 为正数、MARKET 时为 null;止盈/止损 style 为 MARKET 时对应 limit_price 为 null。
- 可选: algo_type(DIRECT / TWAP / VWAP / POV / ICEBERG)、algo_params(不适用时省略或置 null)。

共同要求:两个分支字段不可混写;不要输出契约之外的任何字段;标的必须来自任务输入/上游允许范围,不得改写代码。`
