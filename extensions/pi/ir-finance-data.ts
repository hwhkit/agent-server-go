import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { readFile, realpath } from "node:fs/promises";

const TOOL_NAME = "ir_finance_snapshot";
const MARKET_TOOL_NAME = "ir_market_history";
const SOURCE_TIMEOUT_MS = 15_000;
const MAX_SOURCE_BYTES = 16 * 1024 * 1024;
const SEC_USER_AGENT = process.env.IR_FINANCE_USER_AGENT || "IR-Agent-Center/1.0 https://infoman.ai";
const CONFIGURED_SKILL_PATHS = [
  process.env.LOCAL_AGENT__STRATEGY_SKILL_PATHS || "",
  process.env.LOCAL_AGENT__ANALYSIS_SKILL_PATHS || "",
  process.env.LOCAL_AGENT__WORKFLOW_SKILL_PATHS || "",
]
  .flatMap((value) => value.split(","))
  .map((path) => path.trim())
  .filter(Boolean);

const Metrics: Record<string, string[]> = {
  revenue: ["RevenueFromContractWithCustomerExcludingAssessedTax", "SalesRevenueNet", "Revenues"],
  netIncome: ["NetIncomeLoss", "ProfitLoss"],
  operatingIncome: ["OperatingIncomeLoss"],
  assets: ["Assets"],
  liabilities: ["Liabilities"],
  equity: ["StockholdersEquity", "StockholdersEquityIncludingPortionAttributableToNoncontrollingInterest"],
  cash: ["CashAndCashEquivalentsAtCarryingValue", "CashCashEquivalentsRestrictedCashAndRestrictedCashEquivalents"],
  operatingCashFlow: ["NetCashProvidedByUsedInOperatingActivities"],
  capex: ["PaymentsToAcquirePropertyPlantAndEquipment"],
  dilutedEps: ["EarningsPerShareDiluted"],
  sharesOutstanding: ["CommonStockSharesOutstanding", "EntityCommonStockSharesOutstanding"],
};

type JsonObject = Record<string, any>;

function normalizeSymbol(value: string): string {
  const symbol = value.trim().toUpperCase().replace(/\.US$/, "");
  if (!/^[A-Z0-9.-]{1,12}$/.test(symbol)) throw new Error(`Unsupported US equity symbol: ${value}`);
  return symbol;
}

async function fetchJson(url: string, headers: Record<string, string>, parentSignal?: AbortSignal): Promise<any> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), SOURCE_TIMEOUT_MS);
  const abort = () => controller.abort();
  parentSignal?.addEventListener("abort", abort, { once: true });
  try {
    const response = await fetch(url, { headers, signal: controller.signal });
    if (!response.ok) throw new Error(`${new URL(url).host} returned HTTP ${response.status}`);
    const declared = Number(response.headers.get("content-length") || 0);
    if (declared > MAX_SOURCE_BYTES) throw new Error(`${new URL(url).host} response exceeded ${MAX_SOURCE_BYTES} bytes`);
    const text = await response.text();
    if (Buffer.byteLength(text) > MAX_SOURCE_BYTES) throw new Error(`${new URL(url).host} response exceeded ${MAX_SOURCE_BYTES} bytes`);
    return JSON.parse(text);
  } finally {
    clearTimeout(timer);
    parentSignal?.removeEventListener("abort", abort);
  }
}

let tickerMapPromise: Promise<Map<string, { cik: number; title: string }>> | undefined;

async function tickerMap(signal?: AbortSignal): Promise<Map<string, { cik: number; title: string }>> {
  if (!tickerMapPromise) {
    tickerMapPromise = fetchJson(
      "https://www.sec.gov/files/company_tickers.json",
      { "User-Agent": SEC_USER_AGENT, Accept: "application/json", "Accept-Encoding": "gzip, deflate" },
      signal,
    ).then((payload: JsonObject) => {
      const map = new Map<string, { cik: number; title: string }>();
      for (const item of Object.values(payload) as JsonObject[]) {
        if (item?.ticker && Number.isInteger(item?.cik_str)) {
          map.set(String(item.ticker).toUpperCase(), { cik: Number(item.cik_str), title: String(item.title || item.ticker) });
        }
      }
      return map;
    }).catch((error) => {
      tickerMapPromise = undefined;
      throw error;
    });
  }
  return tickerMapPromise;
}

function recentFacts(companyFacts: JsonObject): JsonObject {
  const gaap = companyFacts?.facts?.["us-gaap"] || {};
  const output: JsonObject = {};
  for (const [metric, tags] of Object.entries(Metrics)) {
    // 同一指标可能跨多个 taxonomy tag 延续披露。不能简单取第一个存在的 tag，
    // 否则会命中已停用的旧标签（例如公司迁移到新 revenue tag 后仍保留历史事实）。
    const concept = tags
      .map((tag) => gaap[tag])
      .filter(Boolean)
      .map((candidate) => {
        const latestFiled = Object.values(candidate.units || {})
          .flatMap((facts: any) => Array.isArray(facts) ? facts : [])
          .filter((fact: JsonObject) => ["10-K", "10-Q", "20-F", "40-F", "6-K"].includes(String(fact.form || "")))
          .reduce((latest: string, fact: JsonObject) => String(fact.filed || fact.end || "") > latest ? String(fact.filed || fact.end || "") : latest, "");
        return { candidate, latestFiled };
      })
      .sort((a, b) => b.latestFiled.localeCompare(a.latestFiled))[0]?.candidate;
    if (!concept?.units) continue;
    const preferredUnit = ["USD", "shares", "USD/shares", "pure"].find((unit) => Array.isArray(concept.units[unit]));
    const unit = preferredUnit || Object.keys(concept.units)[0];
    const facts = Array.isArray(concept.units[unit]) ? concept.units[unit] : [];
    const seen = new Set<string>();
    const latest = facts
      .filter((fact: JsonObject) => ["10-K", "10-Q", "20-F", "40-F", "6-K"].includes(String(fact.form || "")))
      .sort((a: JsonObject, b: JsonObject) => String(b.filed || b.end || "").localeCompare(String(a.filed || a.end || "")))
      .filter((fact: JsonObject) => {
        const key = `${fact.accn || ""}|${fact.end || ""}|${fact.fp || ""}`;
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      })
      .slice(0, 4)
      .map((fact: JsonObject) => ({
        value: fact.val,
        unit,
        fiscalYear: fact.fy ?? null,
        fiscalPeriod: fact.fp ?? null,
        form: fact.form ?? null,
        periodStart: fact.start ?? null,
        periodEnd: fact.end ?? null,
        filedAt: fact.filed ?? null,
        accessionNumber: fact.accn ?? null,
      }));
    if (latest.length) output[metric] = { taxonomyTag: concept.tag || tags[0], label: concept.label || metric, values: latest };
  }
  return output;
}

function recentFilings(submissions: JsonObject, cik: number): JsonObject[] {
  const recent = submissions?.filings?.recent || {};
  const forms: unknown[] = Array.isArray(recent.form) ? recent.form : [];
  const output: JsonObject[] = [];
  for (let index = 0; index < forms.length && output.length < 8; index += 1) {
    const form = String(forms[index] || "");
    if (!["10-K", "10-Q", "8-K", "20-F", "40-F", "6-K"].includes(form)) continue;
    const accessionNumber = String(recent.accessionNumber?.[index] || "");
    const primaryDocument = String(recent.primaryDocument?.[index] || "");
    const accessionPath = accessionNumber.replace(/-/g, "");
    output.push({
      form,
      filingDate: recent.filingDate?.[index] || null,
      reportDate: recent.reportDate?.[index] || null,
      accessionNumber,
      primaryDocument,
      url: accessionPath && primaryDocument
        ? `https://www.sec.gov/Archives/edgar/data/${cik}/${accessionPath}/${primaryDocument}`
        : null,
    });
  }
  return output;
}

function compactNasdaqInfo(payload: JsonObject): JsonObject | null {
  const data = payload?.data;
  if (!data) return null;
  return {
    symbol: data.symbol,
    companyName: data.companyName,
    exchange: data.exchange,
    marketStatus: data.marketStatus,
    primaryData: data.primaryData,
    secondaryData: data.secondaryData,
  };
}

function compactNasdaqSummary(payload: JsonObject): JsonObject | null {
  const summary = payload?.data?.summaryData;
  if (!summary) return null;
  return Object.fromEntries(
    Object.entries(summary).map(([key, value]: [string, any]) => [key, value?.value ?? value]),
  );
}

async function settled<T>(source: string, request: Promise<T>): Promise<{ source: string; ok: true; data: T } | { source: string; ok: false; error: string }> {
  try {
    return { source, ok: true, data: await request };
  } catch (error: any) {
    return { source, ok: false, error: error?.message || String(error) };
  }
}

async function snapshot(symbol: string, cik: number, title: string, signal?: AbortSignal): Promise<JsonObject> {
  const paddedCik = String(cik).padStart(10, "0");
  const secHeaders = { "User-Agent": SEC_USER_AGENT, Accept: "application/json", "Accept-Encoding": "gzip, deflate" };
  const nasdaqHeaders = { "User-Agent": "Mozilla/5.0 (compatible; IR-Agent-Center/1.0)", Accept: "application/json" };
  const [facts, filings, quote, summary] = await Promise.all([
    settled("SEC_COMPANY_FACTS", fetchJson(`https://data.sec.gov/api/xbrl/companyfacts/CIK${paddedCik}.json`, secHeaders, signal)),
    settled("SEC_SUBMISSIONS", fetchJson(`https://data.sec.gov/submissions/CIK${paddedCik}.json`, secHeaders, signal)),
    settled("NASDAQ_QUOTE", fetchJson(`https://api.nasdaq.com/api/quote/${encodeURIComponent(symbol)}/info?assetclass=stocks`, nasdaqHeaders, signal)),
    settled("NASDAQ_SUMMARY", fetchJson(`https://api.nasdaq.com/api/quote/${encodeURIComponent(symbol)}/summary?assetclass=stocks`, nasdaqHeaders, signal)),
  ]);
  return {
    symbol: `${symbol}.US`,
    companyName: title,
    cik: paddedCik,
    quote: quote.ok ? compactNasdaqInfo(quote.data) : { error: quote.error },
    marketSummary: summary.ok ? compactNasdaqSummary(summary.data) : { error: summary.error },
    financialFacts: facts.ok ? recentFacts(facts.data) : { error: facts.error },
    recentFilings: filings.ok ? recentFilings(filings.data, cik) : { error: filings.error },
    sources: [facts, filings, quote, summary].map((item) => ({ source: item.source, ok: item.ok, ...(item.ok ? {} : { error: item.error }) })),
  };
}

function numberValue(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value !== "string") return null;
  const normalized = value.replace(/[$,%\s]/g, "").replace(/,/g, "");
  if (!normalized || normalized === "N/A" || normalized === "--") return null;
  const number = Number(normalized);
  return Number.isFinite(number) ? number : null;
}

function isoDateDaysAgo(days: number): string {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() - days);
  return date.toISOString().slice(0, 10);
}

function round(value: number | null, digits = 4): number | null {
  if (value == null || !Number.isFinite(value)) return null;
  const factor = 10 ** digits;
  return Math.round(value * factor) / factor;
}

function technicalIndicators(bars: JsonObject[]): JsonObject {
  const closes = bars.map((bar) => bar.close as number);
  const volumes = bars.map((bar) => bar.volume as number).filter(Number.isFinite);
  const average = (values: number[]) => values.length ? values.reduce((sum, value) => sum + value, 0) / values.length : null;
  const sma = (period: number) => closes.length >= period ? average(closes.slice(-period)) : null;
  const recent20 = bars.slice(-20);
  const gains: number[] = [];
  const losses: number[] = [];
  for (let index = Math.max(1, closes.length - 14); index < closes.length; index += 1) {
    const change = closes[index] - closes[index - 1];
    gains.push(Math.max(0, change));
    losses.push(Math.max(0, -change));
  }
  const avgGain = average(gains);
  const avgLoss = average(losses);
  const rsi14 = avgGain == null || avgLoss == null
    ? null
    : avgLoss === 0
      ? 100
      : 100 - 100 / (1 + avgGain / avgLoss);
  const trueRanges: number[] = [];
  for (let index = Math.max(1, bars.length - 14); index < bars.length; index += 1) {
    const bar = bars[index];
    const previousClose = bars[index - 1].close as number;
    trueRanges.push(Math.max(
      (bar.high as number) - (bar.low as number),
      Math.abs((bar.high as number) - previousClose),
      Math.abs((bar.low as number) - previousClose),
    ));
  }
  const last = bars.at(-1);
  const prior20 = bars.length > 20 ? bars[bars.length - 21] : bars[0];
  const averageVolume20 = average(volumes.slice(-20));
  return {
    sma20: round(sma(20)),
    sma50: round(sma(50)),
    rsi14: round(rsi14),
    atr14: round(average(trueRanges)),
    return20Pct: last && prior20 ? round((((last.close as number) / (prior20.close as number)) - 1) * 100) : null,
    high20: recent20.length ? round(Math.max(...recent20.map((bar) => bar.high as number))) : null,
    low20: recent20.length ? round(Math.min(...recent20.map((bar) => bar.low as number))) : null,
    averageVolume20: round(averageVolume20, 0),
    latestVolumeRatio20: last && averageVolume20 ? round((last.volume as number) / averageVolume20) : null,
  };
}

async function marketHistory(symbol: string, lookbackDays: number, signal?: AbortSignal): Promise<JsonObject> {
  const nasdaqHeaders = { "User-Agent": "Mozilla/5.0 (compatible; IR-Agent-Center/1.0)", Accept: "application/json" };
  const historyUrl = `https://api.nasdaq.com/api/quote/${encodeURIComponent(symbol)}/historical?assetclass=stocks&fromdate=${isoDateDaysAgo(lookbackDays)}&limit=200`;
  const quoteUrl = `https://api.nasdaq.com/api/quote/${encodeURIComponent(symbol)}/info?assetclass=stocks`;
  const [history, quote] = await Promise.all([
    settled("NASDAQ_HISTORICAL", fetchJson(historyUrl, nasdaqHeaders, signal)),
    settled("NASDAQ_QUOTE", fetchJson(quoteUrl, nasdaqHeaders, signal)),
  ]);
  const rows = history.ok && Array.isArray(history.data?.data?.tradesTable?.rows)
    ? history.data.data.tradesTable.rows
    : [];
  const bars = rows
    .map((row: JsonObject) => ({
      date: row.date,
      open: numberValue(row.open),
      high: numberValue(row.high),
      low: numberValue(row.low),
      close: numberValue(row.close),
      volume: numberValue(row.volume),
    }))
    .filter((bar: JsonObject) => bar.date && [bar.open, bar.high, bar.low, bar.close, bar.volume].every((value) => value != null))
    .reverse()
    .slice(-80);
  return {
    symbol: `${symbol}.US`,
    asOf: new Date().toISOString(),
    quote: quote.ok && quote.data?.data ? compactNasdaqInfo(quote.data) : { error: quote.ok ? "No quote data" : quote.error },
    bars,
    indicators: bars.length >= 2 ? technicalIndicators(bars) : {},
    sources: [history, quote].map((item) => {
      const ok = item.ok && (item.source !== "NASDAQ_HISTORICAL" || bars.length > 0);
      return { source: item.source, ok, ...(ok ? {} : { error: item.ok ? "No OHLCV rows returned" : item.error }) };
    }),
    error: bars.length ? null : (history.ok ? "No OHLCV rows returned for this symbol" : history.error),
  };
}

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "read",
    label: "Read Strategy Skill",
    description: "Read only one of the explicitly configured strategist SKILL.md files. All other filesystem paths are denied.",
    parameters: Type.Object({
      path: Type.String({ description: "Exact configured SKILL.md path" }),
      offset: Type.Optional(Type.Number({ minimum: 1, description: "First line, 1-based" })),
      limit: Type.Optional(Type.Number({ minimum: 1, maximum: 1000, description: "Maximum lines" })),
    }),
    async execute(_toolCallId, params) {
      if (!CONFIGURED_SKILL_PATHS.length) throw new Error("No Agent Skill paths are configured");
      const [requested, allowed] = await Promise.all([
        realpath(params.path),
        Promise.all(CONFIGURED_SKILL_PATHS.map((path) => realpath(path))),
      ]);
      if (!allowed.includes(requested)) throw new Error("Reading this path is not permitted");
      const content = await readFile(requested, "utf8");
      if (Buffer.byteLength(content) > 64 * 1024) throw new Error("Strategy Skill exceeds 64 KiB");
      const lines = content.split("\n");
      const offset = Math.max(1, Math.round(params.offset ?? 1));
      const limit = Math.max(1, Math.min(1000, Math.round(params.limit ?? 500)));
      const selected = lines.slice(offset - 1, offset - 1 + limit).join("\n");
      return {
        content: [{ type: "text", text: selected }],
        details: { path: requested, offset, linesReturned: Math.min(limit, Math.max(0, lines.length - offset + 1)) },
      };
    },
  });

  pi.registerTool({
    name: TOOL_NAME,
    label: "IR Finance Snapshot",
    description: "Fetch current US equity quote/market summary plus authoritative SEC company facts and recent filings. Use before producing fundamental assessments, especially when symbolPool is empty in SEED mode. Accepts at most four US symbols per call.",
    promptSnippet: "Fetch public US equity quote, SEC financial facts, and recent filings for up to four symbols",
    promptGuidelines: [
      "For FUNDAMENTAL_ANALYST work, call ir_finance_snapshot before assigning rating or fair_value.",
      "When SEED mode has an empty symbolPool, first choose up to three liquid US candidates within targetMarkets, then fetch them in one call.",
      "Treat source errors and stale timestamps as data-quality limits; never invent missing financial values.",
    ],
    parameters: Type.Object({
      symbols: Type.Array(Type.String({ description: "US equity ticker, with or without .US suffix" }), {
        minItems: 1,
        maxItems: 4,
        description: "US equity symbols to research in one batch",
      }),
    }),
    async execute(_toolCallId, params, signal) {
      const symbols = [...new Set(params.symbols.map(normalizeSymbol))];
      const map = await tickerMap(signal);
      const resolved = symbols.map((symbol) => {
        const company = map.get(symbol);
        if (!company) throw new Error(`SEC ticker mapping not found for ${symbol}`);
        return { symbol, ...company };
      });
      const results = await Promise.all(resolved.map((item) => snapshot(item.symbol, item.cik, item.title, signal)));
      const payload = {
        schemaVersion: "1.0",
        asOf: new Date().toISOString(),
        dataPolicy: {
          sec: "Authoritative public filing/XBRL data; no API key; subject to SEC fair-access limits.",
          nasdaq: "Public quote pages; availability and latency are best-effort and must be disclosed.",
        },
        results,
      };
      return {
        content: [{ type: "text", text: JSON.stringify(payload) }],
        details: payload,
      };
    },
  });

  pi.registerTool({
    name: MARKET_TOOL_NAME,
    label: "IR Market History",
    description: "Fetch current US equity quote and up to 80 recent daily OHLCV bars, with deterministic SMA/RSI/ATR/volume indicators. Use before producing technical signals. Invalid or unavailable symbols return explicit errors instead of synthetic prices.",
    promptSnippet: "Fetch current quote, daily OHLCV history, and technical indicators for up to four US symbols",
    promptGuidelines: [
      "For TECHNICAL_ANALYST work, call ir_market_history before assigning bias, entry_zone, stop_loss, or targets.",
      "Base trend and key levels on returned bars and indicators; include the data asOf date in reasoning.",
      "If a symbol returns no OHLCV bars, keep it NEUTRAL with an empty entry_zone and explain the source error.",
    ],
    parameters: Type.Object({
      symbols: Type.Array(Type.String({ description: "US equity ticker, with or without .US suffix" }), {
        minItems: 1,
        maxItems: 4,
      }),
      lookbackDays: Type.Optional(Type.Number({ minimum: 30, maximum: 365, description: "Calendar-day lookback; defaults to 180" })),
    }),
    async execute(_toolCallId, params, signal) {
      const symbols = [...new Set(params.symbols.map(normalizeSymbol))];
      const lookbackDays = Math.max(30, Math.min(365, Math.round(params.lookbackDays ?? 180)));
      const results = await Promise.all(symbols.map(async (symbol) => {
        try {
          return await marketHistory(symbol, lookbackDays, signal);
        } catch (error: any) {
          return { symbol: `${symbol}.US`, bars: [], indicators: {}, error: error?.message || String(error) };
        }
      }));
      const payload = {
        schemaVersion: "1.0",
        asOf: new Date().toISOString(),
        interval: "1d",
        results,
      };
      return {
        content: [{ type: "text", text: JSON.stringify(payload) }],
        details: payload,
      };
    },
  });
}
