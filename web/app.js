// Intelligent Router 控制台 SPA（八模块 IA，对接 /api/admin/v2）。
//
// 结构：微型 DOM helper → API 客户端（{"data"}/{"error"} 信封、CSRF、
// If-Match）→ 会话与路由 → 八个模块渲染器 → 启动引导。
// 安全口径：所有服务端数据经 textContent 渲染（无 innerHTML 注入面）；
// 密钥字段只写不读；审计/账号模块按角色隐藏（后端另有强制）。
(() => {
  "use strict";

  // -----------------------------------------------------------------------
  // 微型 DOM helper
  // -----------------------------------------------------------------------
  const h = (tag, props, ...kids) => {
    const el = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === null || value === undefined || value === false) continue;
        if (key === "class") el.className = value;
        else if (key === "style" && typeof value === "object") Object.assign(el.style, value);
        else if (key.startsWith("on") && typeof value === "function") {
          el.addEventListener(key.slice(2).toLowerCase(), value);
        } else if (key === "dataset") Object.assign(el.dataset, value);
        else if (key in el && key !== "list" && typeof value !== "object") el[key] = value;
        else el.setAttribute(key, value);
      }
    }
    for (const kid of kids.flat(Infinity)) {
      if (kid === null || kid === undefined || kid === false) continue;
      el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
    }
    return el;
  };

  const clear = (el) => { while (el.firstChild) el.removeChild(el.firstChild); return el; };
  const frag = (...kids) => kids.flat(Infinity).filter((k) => k !== null && k !== undefined && k !== false);

  // -----------------------------------------------------------------------
  // i18n / 主题 / 本地偏好
  // -----------------------------------------------------------------------
  const THEMES = ["gold", "copper", "silver", "diamond"];
  const prefs = {
    get theme() {
      const saved = localStorage.getItem("ir-theme");
      return THEMES.includes(saved) ? saved : "gold";
    },
    set theme(value) {
      localStorage.setItem("ir-theme", value);
      document.documentElement.dataset.theme = value;
    },
    get lang() {
      const saved = localStorage.getItem("ir-lang");
      return saved === "en" ? "en" : "zh";
    },
    set lang(value) { localStorage.setItem("ir-lang", value); },
    get collapsed() { return localStorage.getItem("ir-sidebar") === "collapsed"; },
    set collapsed(value) {
      localStorage.setItem("ir-sidebar", value ? "collapsed" : "expanded");
    },
  };

  const t = (key, vars) => {
    const dict = window.I18N[prefs.lang] || window.I18N.zh;
    let text = dict[key] !== undefined ? dict[key]
      : (window.I18N.zh[key] !== undefined ? window.I18N.zh[key] : key);
    if (vars) for (const [name, value] of Object.entries(vars)) text = text.replaceAll(`{${name}}`, value);
    return text;
  };

  const applyTheme = () => { document.documentElement.dataset.theme = prefs.theme; };

  // -----------------------------------------------------------------------
  // Toast
  // -----------------------------------------------------------------------
  const toast = (message, kind = "info", timeout = 3800) => {
    const root = document.getElementById("toasts");
    const el = h("div", { class: `toast ${kind}` }, message);
    root.append(el);
    setTimeout(() => el.remove(), timeout);
  };

  // -----------------------------------------------------------------------
  // API 客户端
  // -----------------------------------------------------------------------
  const API_BASE = "/api/admin/v2";

  class ApiError extends Error {
    constructor(status, code, message) {
      super(message || code || `HTTP ${status}`);
      this.status = status;
      this.code = code;
    }
  }

  const api = {
    csrf: "",
    async request(method, path, { body, ifMatch, raw } = {}) {
      const headers = {};
      if (body !== undefined) headers["Content-Type"] = "application/json";
      if (this.csrf && method !== "GET" && method !== "HEAD") headers["X-CSRF-Token"] = this.csrf;
      if (ifMatch !== undefined && ifMatch !== null) headers["If-Match"] = `"${ifMatch}"`;
      const response = await fetch(API_BASE + path, {
        method,
        headers,
        credentials: "same-origin",
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (response.status === 204) return null;
      let envelope = null;
      if (!raw) {
        try { envelope = await response.json(); } catch { envelope = null; }
      }
      if (!response.ok) {
        const err = (envelope && envelope.error) || {};
        throw new ApiError(response.status, err.code, err.message);
      }
      return envelope ? envelope.data : response;
    },
    get: (path, opts) => api.request("GET", path, opts),
    post: (path, body, opts) => api.request("POST", path, { ...opts, body }),
    put: (path, body, opts) => api.request("PUT", path, { ...opts, body }),
    del: (path, opts) => api.request("DELETE", path, opts),
  };

  // 统一错误出口：401 回登录页，其余 toast。
  const reportError = (err, fallbackKey = "common.error") => {
    if (err instanceof ApiError && err.status === 401) {
      state.session = null;
      renderApp();
      toast(err.message || t(fallbackKey), "err");
      return;
    }
    toast(err.message || t(fallbackKey), "err", 6000);
  };

  // -----------------------------------------------------------------------
  // 会话状态
  // -----------------------------------------------------------------------
  const state = {
    session: null, // {username, role}
    route: location.hash.slice(1) || "/dashboard",
    badges: {},
    cache: {}, // 模块内共享的下拉数据（positions/agents）
  };
  const isAdmin = () => state.session && state.session.role === "admin";

  // -----------------------------------------------------------------------
  // 格式化
  // -----------------------------------------------------------------------
  const fmtTime = (value) => {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString(prefs.lang === "zh" ? "zh-CN" : "en-US", { hour12: false });
  };
  // copyText 写剪贴板；非安全上下文（http 远程部署）退回 execCommand。
  const copyText = async (text) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = h("textarea", { style: { position: "fixed", opacity: "0" } });
      ta.value = text;
      document.body.append(ta);
      ta.select();
      document.execCommand("copy");
      ta.remove();
    }
  };
  const fmtDuration = (ms) => {    if (ms === null || ms === undefined) return "—";
    if (ms < 1000) return `${ms}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
  };
  const dictHas = (key) => {
    const dict = window.I18N[prefs.lang] || window.I18N.zh;
    return dict[key] !== undefined || window.I18N.zh[key] !== undefined;
  };
  const statusChip = (status) => h("span", { class: `status ${status || ""}` },
    status ? (dictHas(`status.${status}`) ? t(`status.${status}`) : status) : "—");
  const prettyJSON = (value) => {
    if (value === null || value === undefined || value === "") return "—";
    if (typeof value === "string") {
      try { return JSON.stringify(JSON.parse(value), null, 2); } catch { return value; }
    }
    try { return JSON.stringify(value, null, 2); } catch { return String(value); }
  };
  const pre = (value) => h("pre", { class: "mono", style: { whiteSpace: "pre-wrap", wordBreak: "break-all", margin: "4px 0", maxHeight: "260px", overflow: "auto" } }, prettyJSON(value));

  // -----------------------------------------------------------------------
  // 通用组件
  // -----------------------------------------------------------------------
  const panel = (title, sub, ...kids) => h("section", { class: "panel" },
    title ? h("h2", { class: "panel-title" }, title) : null,
    sub ? h("p", { class: "panel-sub" }, sub) : null,
    ...kids);

  const table = (columns, rows) => {
    const el = h("table", { class: "list" });
    el.append(h("thead", {}, h("tr", {}, columns.map((c) => h("th", {}, c.label)))));
    const tbody = h("tbody");
    for (const row of rows) tbody.append(row);
    el.append(tbody);
    return el;
  };

  const emptyRow = (columns, text) =>
    h("tr", {}, h("td", { colspan: columns, class: "empty" }, text || t("common.empty")));

  const field = (label, input, opts = {}) =>
    h("label", { class: `field ${opts.full ? "full" : ""}` }, h("span", {}, label), input);

  const textInput = (value, attrs = {}) => {
    const el = h("input", { type: "text", ...attrs });
    if (value !== null && value !== undefined) el.value = value;
    return el;
  };

  const selectInput = (options, value) => {
    const el = h("select");
    for (const option of options) {
      const item = h("option", { value: option.value }, option.label);
      if (String(option.value) === String(value)) item.selected = true;
      el.append(item);
    }
    return el;
  };

  const checkbox = (checked, label) => {
    const box = h("input", { type: "checkbox" });
    box.checked = !!checked;
    return label ? h("label", { class: "check" }, box, label) : box;
  };

  // 模态：openModal(title, contentEl, actions) → 关闭函数。
  const openModal = (title, content, actions = [], opts = {}) => {
    const root = document.getElementById("modal-root");
    const close = () => backdrop.remove();
    const backdrop = h("div", {
      class: "modal-backdrop",
      onclick: (event) => { if (event.target === backdrop && opts.dismissable !== false) close(); },
    },
      h("div", { class: `modal ${opts.wide ? "wide" : ""}` },
        h("div", { class: "modal-head" },
          h("div", { class: "modal-title" }, title),
          h("button", { class: "modal-close", title: t("common.close"), onclick: close }, "×")),
        content,
        actions.length
          ? h("div", { class: "modal-actions" },
            actions.map((a) => h("button", { class: `btn ${a.kind || ""}`, onclick: () => a.onClick(close) }, a.label)))
          : null));
    root.append(backdrop);
    return close;
  };

  const confirmDelete = (name, onConfirm) => {
    openModal(t("common.delete"),
      h("p", {}, t("common.confirm.delete", { name })),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        {
          label: t("common.delete"), kind: "primary danger",
          onClick: async (close) => { close(); await onConfirm(); },
        },
      ]);
  };

  // 下拉数据缓存（岗位/智能体选择器共用）。智能体下拉有界（200）：千级
  // 规模下 select 全量加载不可行，更多条目走列表页的搜索式选择。
  const loadCatalog = async () => {
    const [positions, agents] = await Promise.all([
      api.get("/positions?include_disabled=true"),
      api.get("/agents?include_disabled=true&limit=200"),
    ]);
    state.cache.positions = positions || [];
    state.cache.agents = (agents && agents.items) || [];
    return state.cache;
  };

  // -----------------------------------------------------------------------
  // 导航
  // -----------------------------------------------------------------------
  const NAV = [
    { group: "business", route: "/dashboard", icon: "◈", key: "nav.dashboard" },
    { group: "business", route: "/agents", icon: "⬡", key: "nav.agents" },
    { group: "business", route: "/teams", icon: "⬢", key: "nav.teams", badge: (b) => b.activeTeamRuns },
    { group: "business", route: "/logs", icon: "≣", key: "nav.logs", badge: (b) => b.runsToday },
    { group: "system", route: "/users", icon: "☖", key: "nav.users", adminOnly: true },
    { group: "system", route: "/settings", icon: "⚙", key: "nav.settings" },
  ];

  const MODULE_TITLES = {
    "/dashboard": "dash.title", "/positions": "pos.title", "/agents": "agent.title",
    "/teams": "team.title", "/logs": "nav.logs",
    "/users": "user.title", "/settings": "set.title",
  };

  const navigate = (route) => {
    location.hash = "#" + route;
  };

  // -----------------------------------------------------------------------
  // 模块：登录
  // -----------------------------------------------------------------------

  // irLogoSVG 是品牌 logo：六边形路由网络（中心路由节点 + 三颗卫星智能体
  // 节点 + 电路走线端口），金色渐变描边，卫星节点脉冲动画。静态常量标记
  // （无用户数据），innerHTML 注入安全。
  const irLogoSVG = (size = 52) => {
    const wrap = document.createElement("div");
    wrap.className = "ir-logo";
    wrap.innerHTML = `
<svg viewBox="0 0 96 96" width="${size}" height="${size}" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
  <defs>
    <linearGradient id="irg" x1="14" y1="10" x2="82" y2="88" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="#f4dd7a"/>
      <stop offset=".55" stop-color="#d4af37"/>
      <stop offset="1" stop-color="#8a6d1f"/>
    </linearGradient>
    <radialGradient id="irp" cx="0" cy="0" r="1" gradientUnits="userSpaceOnUse"
      gradientTransform="translate(48 48) scale(10)">
      <stop offset="0" stop-color="#f8e9b0"/>
      <stop offset="1" stop-color="#d4af37" stop-opacity="0"/>
    </radialGradient>
  </defs>
  <path d="M48 8 82 27.5v39L48 86 14 66.5v-39L48 8Z"
    stroke="url(#irg)" stroke-width="3" stroke-linejoin="round" opacity=".95"/>
  <path d="M48 17 74 32v32L48 79 22 64V32L48 17Z"
    stroke="url(#irg)" stroke-width="1" opacity=".35"/>
  <circle cx="48" cy="48" r="10" fill="url(#irp)"/>
  <circle cx="48" cy="48" r="4.2" fill="#f4dd7a"/>
  <circle cx="48" cy="48" r="6.5" stroke="url(#irg)" stroke-width="1.6"/>
  <g stroke="url(#irg)" stroke-width="1.6" opacity=".9">
    <path d="M48 41.5 33 26"/>
    <path d="M57 52l17 6"/>
    <path d="M52 56l-4 15"/>
  </g>
  <g fill="#f4dd7a">
    <circle cx="33" cy="26" r="3">
      <animate attributeName="r" values="3;4.2;3" dur="2.4s" repeatCount="indefinite"/>
      <animate attributeName="opacity" values="1;.5;1" dur="2.4s" repeatCount="indefinite"/>
    </circle>
    <circle cx="74" cy="58" r="3">
      <animate attributeName="r" values="3;4.2;3" dur="2.4s" begin=".8s" repeatCount="indefinite"/>
      <animate attributeName="opacity" values="1;.5;1" dur="2.4s" begin=".8s" repeatCount="indefinite"/>
    </circle>
    <circle cx="48" cy="71" r="3">
      <animate attributeName="r" values="3;4.2;3" dur="2.4s" begin="1.6s" repeatCount="indefinite"/>
      <animate attributeName="opacity" values="1;.5;1" dur="2.4s" begin="1.6s" repeatCount="indefinite"/>
    </circle>
  </g>
  <g stroke="url(#irg)" stroke-width="1.4" opacity=".8">
    <path d="M14 40H6"/><path d="M14 54H6"/>
    <path d="M82 40h8"/><path d="M82 54h8"/>
    <path d="M40 86v8"/><path d="M56 86v8"/>
  </g>
  <g fill="#d4af37">
    <rect x="4" y="38.2" width="4" height="3.6" rx=".8"/>
    <rect x="4" y="52.2" width="4" height="3.6" rx=".8"/>
    <rect x="88" y="38.2" width="4" height="3.6" rx=".8"/>
    <rect x="88" y="52.2" width="4" height="3.6" rx=".8"/>
    <rect x="38.2" y="88" width="3.6" height="4" rx=".8"/>
    <rect x="54.2" y="88" width="3.6" height="4" rx=".8"/>
  </g>
</svg>`;
    return wrap;
  };
  const renderLogin = (root) => {
    const username = textInput("", { placeholder: t("login.username"), autocomplete: "username" });
    const password = h("input", { type: "password", placeholder: t("login.password"), autocomplete: "current-password" });
    const errorEl = h("div", { class: "err-text" });
    const submit = async (event) => {
      event.preventDefault();
      clear(errorEl);
      try {
        const data = await api.post("/login", { username: username.value, password: password.value });
        api.csrf = data.csrfToken || "";
        state.session = { username: data.username, role: data.role };
        state.route = location.hash.slice(1) || "/dashboard";
        renderApp();
        refreshBadges();
      } catch (err) {
        errorEl.textContent = err.message || t("login.failed");
      }
    };
    root.append(h("div", { class: "login-wrap" },
      h("div", { class: "login-card" },
        // 黄金分割双栏：左侧品牌区（正方形 ≈ 整卡高的 1.618 内切），
        // 右侧表单区（余量 0.618）。
        h("div", { class: "login-brand" },
          h("div", { class: "login-logo" }, irLogoSVG(96)),
          h("div", { class: "login-title" }, t("login.title")),
          h("div", { class: "login-sub" }, t("login.sub"))),
        h("div", { class: "login-form" },
          h("form", { onsubmit: submit },
            h("label", { class: "field" }, h("span", {}, t("login.username")), username),
            h("label", { class: "field" }, h("span", {}, t("login.password")), password),
            errorEl,
            h("button", { class: "btn primary", type: "submit" }, t("login.submit")))))));
    username.focus();
  };

  // -----------------------------------------------------------------------
  // 模块：总览
  // -----------------------------------------------------------------------
  const renderDashboard = async (container) => {
    const snap = await api.get("/dashboard");
    state.badges = {
      activeTeamRuns: snap.activeTeamRuns || 0,
      runsToday: sumTotals(snap.runsToday),
      onlineWorkers: snap.onlineWorkers || 0,
    };
    const totals = snap.runsToday || {};
    const stat = (label, value, sub, cls, labelExtra) =>
      h("div", { class: "stat-card" },
        h("div", { class: "label" }, label, labelExtra || null),
        h("div", { class: `value ${cls || ""}`, title: String(value) }, String(value)),
        sub ? h("div", { class: "sub", title: sub }, sub) : null);
    // 版本串拆两行展示：`v0.1-34` / `gaa7b9f8-dirty`，避免长串截断。
    const [versionMain, versionRest] =
      (/^(v?\d+(?:\.\d+)*(?:-\d+)?)-(.+)$/.exec(snap.version || "") || [null, snap.version || "—", null]).slice(1);
    container.append(
      h("div", { class: "stat-grid mb" },
        stat(t("dash.version"), versionMain, versionRest, "mono"),
        stat(t("dash.onlineWorkers"), snap.onlineWorkers ?? 0,
          `${t("dash.workersTotal")} ${snap.workersTotal ?? 0}`),
        stat(t("dash.queue"), `${(snap.queue && snap.queue.Queued) || 0} / ${(snap.queue && snap.queue.Leased) || 0}`,
          `${t("dash.queue.queued")} / ${t("dash.queue.leased")}`),
        stat(t("dash.activeTeamRuns"), snap.activeTeamRuns ?? 0,
          `${t("status.running")} ${snap.teamRunsRunning ?? 0} · ${t("status.queued")} ${snap.teamRunsQueued ?? 0}`),
        stat(t("dash.runsToday"), sumTotals(totals),
          `${t("dash.completed")} ${totals.Completed || 0} · ${t("dash.failed")} ${totals.Failed || 0}`,
          null, h("span", { class: "label-time mono", title: fmtTime(snap.serverTime) }, fmtTime(snap.serverTime)))));
    updateBadges();
  };

  const sumTotals = (totals) => {
    if (!totals) return 0;
    return (totals.Completed || 0) + (totals.Failed || 0) + (totals.Canceled || 0) +
      (totals.InProgress || 0) + (totals.Queued || 0);
  };

  // -----------------------------------------------------------------------
  // 模块：岗位
  // -----------------------------------------------------------------------
  const BUILTIN_POSITIONS = new Set([
    "FUNDAMENTAL_ANALYST", "TECHNICAL_ANALYST", "STRATEGIST", "RISK_MANAGER",
    "EXECUTION_TRADER", "PORTFOLIO_MANAGER", "__CUSTOM__",
  ]);

  const renderPositions = async (container) => {
    const items = await api.get("/positions?include_disabled=true");
    const columns = [
      { label: t("pos.name") }, { label: t("pos.description") },
      { label: t("pos.code") }, { label: t("pos.contractRef") }, { label: t("common.enabled") },
      { label: t("common.actions") },
    ];
    const rows = (items || []).map((p) => h("tr", {},
      h("td", {}, p.name),
      h("td", { class: "muted", style: { maxWidth: "320px" } }, p.description || "—"),
      h("td", { class: "mono" }, p.code),
      h("td", { class: "mono muted small" }, p.contract_ref),
      h("td", {}, p.enabled ? h("span", { class: "dot on" }) : h("span", { class: "dot off" })),
      h("td", { class: "nowrap" },
        h("button", { class: "btn sm", onclick: () => positionEditor(p) }, t("common.edit")),
        " ",
        h("button", {
          class: "btn sm danger",
          disabled: BUILTIN_POSITIONS.has(p.code),
          title: BUILTIN_POSITIONS.has(p.code) ? "golden contract" : "",
          onclick: () => confirmDelete(p.code, async () => {
            try {
              await api.del(`/positions/${encodeURIComponent(p.code)}`, { ifMatch: p.revision });
              toast(t("common.deleted"), "ok");
              rerender();
            } catch (err) { reportError(err); }
          }),
        }, t("common.delete")))));
    if (!rows.length) rows.push(emptyRow(columns.length));
    container.append(panel(null, null,
      h("div", { class: "row mb" },
        h("button", { class: "btn primary", onclick: () => positionEditor(null) }, `+ ${t("pos.new")}`)),
      table(columns, rows)));
  };

  const positionEditor = (existing) => {
    const code = textInput(existing ? existing.code : "", { placeholder: "ANALYST_X" });
    if (existing) code.readOnly = true;
    const name = textInput(existing ? existing.name : "");
    const description = textInput(existing ? existing.description : "");
    const contractRef = textInput(existing ? existing.contract_ref : "custom:");
    const outputSchema = h("textarea", { rows: 4, placeholder: "{…}" });
    if (existing && existing.output_schema) outputSchema.value = prettyJSON(existing.output_schema);
    const enabled = checkbox(existing ? existing.enabled : true, t("common.enabled"));
    const save = async (close) => {
      let schema = null;
      const rawSchema = outputSchema.value.trim();
      if (rawSchema) {
        try { schema = JSON.parse(rawSchema); }
        catch { toast("output_schema: invalid JSON", "err"); return; }
      }
      const body = {
        code: code.value.trim(), name: name.value.trim(),
        description: description.value.trim(), contract_ref: contractRef.value.trim(),
        output_schema: schema, enabled: enabled.checked,
      };
      try {
        if (existing) {
          await api.put(`/positions/${encodeURIComponent(existing.code)}`, body, { ifMatch: existing.revision });
        } else {
          await api.post("/positions", body);
        }
        close();
        toast(t("common.saved"), "ok");
        rerender();
      } catch (err) { reportError(err); }
    };
    openModal(existing ? t("pos.edit") : t("pos.new"),
      h("div", { class: "form-grid" },
        field(t("pos.code"), code),
        field(t("pos.name"), name),
        field(t("pos.description"), description, { full: true }),
        field(t("pos.contractRef"), contractRef),
        field(t("common.enabled"), enabled),
        field(t("pos.outputSchema"), outputSchema, { full: true })),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("common.save"), kind: "primary", onClick: save },
      ]);
  };

  // -----------------------------------------------------------------------
  // 模块：智能体（子页签：智能体 / 执行后端 / 连接器）
  // -----------------------------------------------------------------------
  const renderAgents = async (container) => {
    // 岗位融合为智能体模块的子页签（侧栏不再单列）。
    const tabs = tabsEl([
      { key: "agents", label: t("agent.tab.agents"), render: agentsTab },
      { key: "positions", label: t("pos.title"), render: renderPositions },
      { key: "providers", label: t("agent.tab.providers"), render: providersTab },
      { key: "connectors", label: t("agent.tab.connectors"), render: connectorsTab },
    ], container.dataset.agentsTab || "agents");
    container.append(tabs.el);
    container.append(tabs.body);
    tabs.activate(container.dataset.agentsTab || "agents");
  };

  const tabsEl = (items, initial) => {
    const bar = h("div", { class: "tabs" });
    const body = h("div");
    const buttons = {};
    const activate = async (key) => {
      for (const item of items) buttons[item.key].classList.toggle("active", item.key === key);
      clear(body);
      const item = items.find((i) => i.key === key) || items[0];
      try {
        await item.render(body);
      } catch (err) {
        clear(body);
        body.append(h("div", { class: "err-text" }, err.message));
      }
    };
    for (const item of items) {
      buttons[item.key] = h("a", {
        class: "tab", href: "#",
        onclick: (event) => { event.preventDefault(); activate(item.key); },
      }, item.label);
      bar.append(buttons[item.key]);
    }
    return { el: bar, body, activate };
  };

  // ---- 智能体列表 ----
  // 接入弹窗：把智能体绑定到执行节点要复制/核对的三样东西——
  // 接入地址、签名密钥、接口契约（内容与 /agent-api/v1 实现对齐）。
  const connectModal = async (agent) => {
    const base = `${location.origin}/agent-api/v1`;
    const copyRow = (value) => {
      const input = h("input", { class: "mono", type: "text", readonly: true,
        style: { flex: "1", minWidth: "0" } });
      input.value = value;
      return h("div", { class: "row", style: { gap: "8px" } }, input,
        h("button", {
          class: "btn sm", type: "button",
          onclick: async () => { await copyText(value); toast(t("common.copied"), "ok"); },
        }, t("common.copy")));
    };
    const docText = [
      `base = ${base}`,
      "",
      "鉴权（二选一）:",
      "  智能体密钥  Authorization: Bearer <签名密钥>",
      "              X-Agent-Slug: <slug>  （集群子密钥再加 X-Agent-Cluster: <cluster>）",
      "  连接器令牌  Authorization: Bearer act_…  （「连接器」页签签发，可服务多智能体）",
      "",
      "POST /claim  长轮询认领任务",
      `  {"workerId":"w1","maxWaitSeconds":25,"inFlight":0,"capacity":4,"preferredSlugs":["${agent.slug}"]}`,
      '  200 → {"schemaVersion":"agent-api.claim.v1","runId":"…","agentSlug":"…","input":{…},',
      '         "leaseExpireAt":"…","renewAfterSeconds":…}',
      "  204 → 暂无任务（X-Empty-Reason 头说明原因）",
      "POST /renew  续租（headers: X-Agent-Run-ID / X-Agent-Worker-ID）",
      "POST /complete  回传结果（headers 同上）",
      '  {"status":"succeeded|failed","payload":{…},"error":{"code":"…","message":"…"},',
      '   "usage":{"inputTokens":0,"outputTokens":0,"totalTokens":0},"metrics":{"durationMs":0}}',
      "POST /runs  机器侧直接提交运行结果（幂等）",
      `  {"slug":"${agent.slug}","status":"succeeded|failed","runId":"可选","payload":{…},"input":{…}}`,
      "POST /heartbeat  worker 心跳（维持连接状态，上报能力域）",
      `  {"workerId":"w1","inFlight":0,"capacity":4,"capabilities":{"slugs":["${agent.slug}"],"version":"…"}}`,
      "",
      "curl 示例:",
      `curl -sS ${base}/claim \\`,
      `  -H "Authorization: Bearer <签名密钥>" -H "X-Agent-Slug: ${agent.slug}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{"workerId":"w1","maxWaitSeconds":25,"capacity":4,"preferredSlugs":["${agent.slug}"]}'`,
    ].join("\n");
    let secret = "";
    try {
      const data = await api.get(`/agents/${encodeURIComponent(agent.slug)}/secret`);
      secret = data.signing_secret || "";
    } catch { /* 解密失败等：留空并提示 */ }
    openModal(`${t("agent.connect")} · ${agent.slug}`,
      h("div", { class: "form-grid" },
        field(t("agent.slug"), copyRow(agent.slug)),
        field(t("agent.connect.base"), copyRow(base)),
        field(t("agent.secret"), copyRow(secret)),
        field(t("agent.connect.doc"),
          h("div", { class: "row", style: { gap: "8px", alignItems: "flex-start" } },
            h("pre", { class: "api-doc" }, docText),
            h("button", {
              class: "btn sm", type: "button",
              onclick: async () => { await copyText(docText); toast(t("common.copied"), "ok"); },
            }, t("agent.connect.copyDoc"))),
          { full: true })),
      [{ label: t("common.close"), onClick: (close) => close() }],
      { wide: true });
  };

  const agentsTab = async (body) => {
    const { positions } = await loadCatalog();
    // 连接状态数据源：该 slug 名下是否有活跃 worker 心跳（remote_pull
    // 智能体由连接器认领执行——有心跳=连接器在线）。
    // 连接状态数据源：在线 worker 的能力域（slugs）覆盖到的智能体即已连接。
    let workerSlugs = new Set();
    try {
      const workers = await api.get("/agent-connectors/workers");
      for (const worker of workers || []) {
        for (const slug of worker.slugs || []) workerSlugs.add(slug);
      }
    } catch { /* 探测失败降级为全部未连接 */ }
    const columns = [
      { label: t("agent.slug") }, { label: t("agent.name") }, { label: t("agent.status") },
      { label: t("agent.secret") },
      { label: t("common.enabled") }, { label: t("common.actions") },
    ];
    const renderAgentRow = (a) => h("tr", {},
      h("td", {}, h("button", {
        class: "slug-btn mono", type: "button", title: t("agent.connect"),
        onclick: () => connectModal(a),
      }, a.slug)),
      h("td", {}, a.name, a.description ? h("div", { class: "muted small" }, a.description) : null),
      h("td", {}, workerSlugs.has(a.slug)
        ? h("span", { class: "status completed" }, t("agent.status.online"))
        : h("span", { class: "status offline" }, t("agent.status.offline"))),
      h("td", { class: "nowrap" }, a.signing_secret_configured
        ? [h("span", { class: "ok-text small" }, `🔒 ${t("agent.secret.configured")}`), " ",
          h("button", {
            class: "btn sm", type: "button",
            // 从 /agents/{slug}/secret 取解密后的签名密钥并写入剪贴板。
            onclick: async () => {
              try {
                const data = await api.get(`/agents/${encodeURIComponent(a.slug)}/secret`);
                await copyText(data.signing_secret);
                toast(t("common.copied"), "ok");
              } catch (err) { reportError(err); }
            },
          }, t("common.copy"))]
        : h("span", { class: "muted small" }, "—")),
      h("td", {}, a.enabled ? h("span", { class: "dot on" }) : h("span", { class: "dot off" })),
      h("td", { class: "nowrap" },
        h("button", { class: "btn sm", onclick: () => connectModal(a) }, t("agent.connect")),
        " ",
        h("button", { class: "btn sm", onclick: () => agentEditor(a, positions) }, t("common.edit")),
        " ",
        h("button", {
          class: "btn sm danger",
          onclick: () => confirmDelete(a.slug, async () => {
            try {
              await api.del(`/agents/${encodeURIComponent(a.slug)}`, { ifMatch: a.revision });
              toast(t("common.deleted"), "ok");
              rerender();
            } catch (err) { reportError(err); }
          }),
        }, t("common.delete"))));

    // 千级规模：服务端分页 + 搜索（limit/offset/query），本地不缓存全量。
    const pageSize = 30;
    const listWrap = h("div");
    const queryInput = textInput("", { placeholder: `${t("agent.slug")} / ${t("agent.name")}`,
      style: { width: "180px" } });
    const apply = h("button", { class: "btn", onclick: () => load(1) }, t("common.filter"));
    const newBtn = h("button", { class: "btn primary", onclick: () => agentEditor(null, positions) },
      `+ ${t("agent.new")}`);
    // 批量迁移入口（B1）：导出 JSON（迁移，含密文）/CSV（观察），导入粘贴 JSON。
    const exportBtn = (format) => h("button", {
      class: "btn",
      onclick: async () => {
        try {
          const response = await fetch(`/api/admin/v2/agents/export?format=${format}`,
            { credentials: "same-origin" });
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          const blob = await response.blob();
          const url = URL.createObjectURL(blob);
          const link = h("a", { href: url, download: `agents.${format}` });
          link.click();
          URL.revokeObjectURL(url);
        } catch (err) { reportError(err); }
      },
    }, `⬇ ${format.toUpperCase()}`);
    const importBtn = h("button", {
      class: "btn",
      onclick: () => importAgentsModal(),
    }, `⬆ ${t("agent.import")}`);
    body.append(panel(null, null,
      h("div", { class: "row mb" }, queryInput, apply, newBtn,
        h("div", { class: "spacer" }), exportBtn("json"), importBtn),
      listWrap));

    const load = async (page) => {
      const params = new URLSearchParams({ limit: String(pageSize), offset: String((page - 1) * pageSize) });
      if (queryInput.value.trim()) params.set("query", queryInput.value.trim());
      // 始终包含停用智能体：启用列圆点已区分状态，停用项保持可编辑可恢复。
      params.set("include_disabled", "true");
      const data = await api.get(`/agents?${params}`);
      clear(listWrap);
      const rows = (data.items || []).map(renderAgentRow);
      if (!rows.length) rows.push(emptyRow(columns.length));
      listWrap.append(...[table(columns, rows), pager(data.total || 0, page, pageSize, load)].filter(Boolean));
    };
    await load(1);
  };

  // 智能体编辑器（产品定位：调度 + 连接）。执行绑定固定 remote-pull/
  // remote_pull（连接器拉取执行，不占全局执行闸）、签名编码固定 hex——
  // 本地 CLI 执行配置（系统提示词/工作目录/模型/工具/权限模式）不再
  // 属于智能体表单。
  const agentEditor = (existing, positions) => {
    const slug = textInput(existing ? existing.slug : "", { placeholder: "my-agent" });
    if (existing) slug.readOnly = true;
    const name = textInput(existing ? existing.name : "");
    const positionCodes = new Set(existing ? existing.positions || [] : []);
    const positionBoxes = positions.map((p) => {
      // 标签只显示名称；代码放 title 悬停核对（同名自定义岗位靠它区分）。
      const box = h("input", { type: "checkbox" });
      box.checked = positionCodes.has(p.code);
      box.addEventListener("change", () => {
        if (box.checked) positionCodes.add(p.code); else positionCodes.delete(p.code);
      });
      return h("label", { class: "check", title: p.code }, box,
        h("span", {}, p.name));
    });
    const secret = h("input", {
      type: "password",
      placeholder: existing ? t("agent.secret.placeholder.edit") : t("agent.secret.placeholder.new"),
      autocomplete: "new-password",
      style: { flex: "1", minWidth: "0" },
    });
    // 密钥工具行：生成（32 字节随机 hex，即点即填）/ 显示-隐藏（生成的
    // 密钥需要明文交给动态入口调用方去签名）。
    const genSecret = h("button", {
      class: "btn sm", type: "button",
      onclick: () => {
        const bytes = crypto.getRandomValues(new Uint8Array(32));
        secret.value = [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
      },
    }, t("agent.secret.generate"));
    const toggleSecret = h("button", {
      class: "btn sm", type: "button",
      onclick: () => {
        const reveal = secret.type === "password";
        secret.type = reveal ? "text" : "password";
        toggleSecret.textContent = reveal ? t("agent.secret.hide") : t("agent.secret.show");
      },
    }, t("agent.secret.show"));
    const copySecret = h("button", {
      class: "btn sm", type: "button",
      onclick: async () => {
        if (!secret.value.trim()) return;
        try {
          await navigator.clipboard.writeText(secret.value.trim());
          toast(t("common.copied"), "ok");
        } catch {
          // 剪贴板不可用（非安全上下文等）：切明文让用户手选。
          secret.type = "text";
          toggleSecret.textContent = t("agent.secret.hide");
          secret.select();
        }
      },
    }, t("common.copy"));
    const timeout = textInput(existing ? String(existing.timeout_secs ?? "") : "", { type: "number", min: "1" });
    const maxConc = textInput(existing ? String(existing.max_concurrency ?? "") : "", { type: "number", min: "0" });
    const queueCap = textInput(existing ? String(existing.queue_capacity ?? "") : "", { type: "number", min: "0" });
    const enabled = checkbox(existing ? existing.enabled : true, t("common.enabled"));

    const numOrNull = (el) => {
      const value = el.value.trim();
      return value === "" ? null : Number(value);
    };

    const save = async (close) => {
      // 前端预校验（规则与后端一致）：启用态必须聘任至少一个岗位——
      // 避免提交后才收到英文 422。
      if (enabled.checked && positionCodes.size === 0) {
        toast(t("agent.errors.noPositions"), "err", 6000);
        return;
      }
      const body = {
        slug: slug.value.trim(), name: name.value.trim(),
        // 调度 + 连接定位：执行绑定固定远端拉取，签名固定 hex。
        executor_kind: "remote-pull", execution_mode: "remote_pull",
        positions: [...positionCodes],
        signature_encoding: "hex",
        timeout_secs: numOrNull(timeout), max_concurrency: numOrNull(maxConc),
        queue_capacity: numOrNull(queueCap), enabled: enabled.checked,
      };
      if (secret.value.trim()) body.signing_secret = secret.value.trim();
      try {
        if (existing) {
          await api.put(`/agents/${encodeURIComponent(existing.slug)}`, body, { ifMatch: existing.revision });
        } else {
          await api.post("/agents", body);
        }
        close();
        toast(t("common.saved"), "ok");
        rerender();
      } catch (err) { reportError(err); }
    };

    openModal(existing ? `${t("agent.edit")} · ${existing.slug}` : t("agent.new"),
      h("div", { class: "form-grid" },
        field(t("agent.slug"), slug),
        field(t("agent.name"), name),
        field(t("agent.positions"),
          h("div", { class: "check-grid3" }, ...positionBoxes),
          { full: true }),
        field(t("agent.secret"),
          h("div", { class: "row", style: { flexWrap: "nowrap" } },
            secret, genSecret, toggleSecret, copySecret),
          { full: true }),
        field(t("common.enabled"), enabled),
        // 调度约束三件套：单行三列，不再折叠。
        h("div", { class: "form-grid triple full" },
          field(t("agent.timeout"), timeout),
          field(t("agent.maxConc"), maxConc),
          field(t("agent.queueCap"), queueCap)),
      ),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("common.save"), kind: "primary", onClick: save },
      ],
      { wide: true });
  };

// ---- 执行后端（三列卡片） ----
  const providersTab = async (body) => {
    const views = await api.get("/providers");
    const canEdit = isAdmin();
    const cards = views.map((view) => {
      const assignmentChips = (view.assignments || []).map((a) =>
        h("span", { class: "chip", title: `${a.executor_kind} / ${a.execution_mode}` }, a.label));
      return h("div", { class: `provider-card ${view.configured ? "configured" : ""}` },
        h("div", { class: "provider-head" },
          h("span", { class: `dot ${view.configured ? "on" : "off"}` }),
          h("span", { class: "provider-name", title: view.id }, view.name),
          h("span", { class: "chip" }, view.category === "local" ? t("prov.category.local") : t("prov.category.remote"))),
        h("div", { class: "provider-desc" }, view.description || ""),
        h("div", { class: `provider-status ${view.configured ? "ok-text" : "muted"}` }, view.status_detail || ""),
        h("div", { class: "provider-meta small" },
          h("span", { class: "muted" },
            `${t("common.revision")} ${view.revision}`,
            view.secret_configured ? ` · 🔒 ${t("prov.secret")}` : ""),
          canEdit && view.editable
            ? h("button", { class: "btn sm", onclick: () => providerEditor(view) }, t("prov.edit"))
            : null),
        assignmentChips.length
          ? h("div", { class: "provider-assign" },
            h("div", { class: "muted small mb" }, t("prov.assignments")), assignmentChips)
          : null);
    });
    body.append(panel(null,
      canEdit ? t("prov.sub") : `${t("prov.sub")} ${t("prov.readonly")}`,
      cards.length ? h("div", { class: "grid3" }, cards) : h("div", { class: "empty" }, t("common.empty"))));
  };

  const SECRET_FIELD_KEYS = new Set(["api_key", "mcp_authorization"]);

  const providerEditor = (view) => {
    const config = view.config || {};
    const inputs = {};
    const grid = h("div", { class: "form-grid" });
    for (const [key, value] of Object.entries(config)) {
      if (key === "mcp_authorization_configured") continue;
      let input;
      if (SECRET_FIELD_KEYS.has(key)) {
        input = h("input", {
          type: "password", autocomplete: "new-password",
          placeholder: view.secret_configured ? t("agent.secret.placeholder.edit") : "",
        });
      } else if (typeof value === "boolean") {
        input = checkbox(value);
      } else if (Array.isArray(value)) {
        input = textInput((value || []).join(", "), { placeholder: "vault-1, vault-2" });
        input.dataset.array = "1";
      } else if (value !== null && typeof value === "object") {
        input = textInput(JSON.stringify(value));
      } else {
        input = textInput(value === null || value === undefined ? "" : String(value));
        if (typeof value === "number") input.type = "number";
      }
      inputs[key] = input;
      const label = key + (SECRET_FIELD_KEYS.has(key) && view.secret_configured ? " 🔒" : "");
      grid.append(field(label, input,
        { full: key === "vault_ids" || SECRET_FIELD_KEYS.has(key) }));
    }
    const save = async (close) => {
      const body = {};
      for (const [key, input] of Object.entries(inputs)) {
        if (SECRET_FIELD_KEYS.has(key)) {
          // 空 = 保留既有密文（后端 assignSecret preserveEmpty 语义）。
          if (input.value.trim()) body[key] = input.value.trim();
          continue;
        }
        if (input.type === "checkbox") { body[key] = input.checked; continue; }
        if (input.dataset.array) {
          const text = input.value.trim();
          body[key] = text ? text.split(",").map((s) => s.trim()).filter(Boolean) : null;
          continue;
        }
        const raw = input.value.trim();
        if (raw === "") { body[key] = null; continue; }
        if (input.type === "number") { body[key] = Number(raw); continue; }
        body[key] = raw;
      }
      try {
        await api.put(`/providers/${encodeURIComponent(view.id)}`, body, { ifMatch: view.revision });
        close();
        toast(t("common.saved"), "ok");
        rerender();
      } catch (err) { reportError(err); }
    };
    openModal(`${t("prov.edit")} · ${view.name}`, grid, [
      { label: t("common.cancel"), onClick: (close) => close() },
      { label: t("common.save"), kind: "primary", onClick: save },
    ], { wide: true });
  };

  // ---- 连接器 ----
  const connectorsTab = async (body) => {
    const items = await api.get("/agent-connectors");
    const columns = [
      { label: t("conn.name") }, { label: t("conn.scopes") }, { label: t("run.status") },
      { label: t("user.created") }, { label: t("common.actions") },
    ];
    const rows = (items || []).map((c) => h("tr", {},
      h("td", { class: "mono" }, c.name),
      h("td", {}, (c.scopes || []).map((s) => h("span", { class: "chip" }, s))),
      h("td", {}, c.active
        ? h("span", { class: "status completed" }, t("conn.active"))
        : h("span", { class: "status canceled" }, c.revoked_at ? t("conn.revoked") : "—")),
      h("td", { class: "muted small" }, fmtTime(c.created_at)),
      h("td", {}, h("button", {
        class: "btn sm danger",
        onclick: () => confirmDelete(c.name, async () => {
          try {
            await api.del(`/agent-connectors/${encodeURIComponent(c.name)}`);
            toast(t("common.deleted"), "ok");
            rerender();
          } catch (err) { reportError(err); }
        }),
      }, t("conn.revoke")))));
    if (!rows.length) rows.push(emptyRow(columns.length));
    body.append(panel(null, t("conn.sub"),
      h("div", { class: "row mb" },
        h("button", { class: "btn primary", onclick: issueConnectorModal }, `+ ${t("conn.issue")}`)),
      table(columns, rows)));
  };

  // 批量导入对话框：粘贴 /agents/export 导出的 JSON，逐条结果显示。
  const importAgentsModal = () => {
    const payload = h("textarea", { rows: 10,
      placeholder: '{"agents":[{"slug":"…","name":"…","binding":{"executor_kind":"pi","execution_mode":"local"},"positions":["FUNDAMENTAL_ANALYST"],"signing_secret":"…"}]}\n（或粘贴 /agents/export 导出的 JSON；密文迁移用 secret_enc 字段替代 signing_secret）' });
    const summary = h("div", { class: "small mt" });
    const doImport = async (close) => {
      clear(summary);
      let parsed;
      try { parsed = JSON.parse(payload.value); }
      catch (err) { summary.append(h("div", { class: "err-text" }, `JSON: ${err.message}`)); return; }
      const agentsList = Array.isArray(parsed) ? parsed : parsed.agents;
      if (!Array.isArray(agentsList) || !agentsList.length) {
        summary.append(h("div", { class: "err-text" }, t("agent.import.empty")));
        return;
      }
      try {
        const result = await api.post("/agents/import", { agents: agentsList });
        summary.append(h("div", { class: "ok-text" },
          `✓ ${t("agent.import.done")} — created ${result.created} · updated ${result.updated} · failed ${result.failed}`));
        const failures = (result.results || []).filter((item) => item.status === "failed");
        for (const failure of failures.slice(0, 5)) {
          summary.append(h("div", { class: "err-text" }, `${failure.slug}: ${failure.error}`));
        }
      } catch (err) {
        summary.append(h("div", { class: "err-text" }, err.message));
      }
    };
    openModal(t("agent.import"),
      h("div", {},
        field(t("agent.import.payload"), payload, { full: true }), summary),
      [
        { label: t("common.close"), onClick: (close) => close() },
        { label: t("agent.import.submit"), kind: "primary", onClick: doImport },
      ],
      { wide: true });
  };

  const issueConnectorModal = async () => {
    let agents = [];
    try { agents = await api.get("/agents?include_disabled=true"); } catch { /* 选择器可空 */ }
    const name = textInput("", { placeholder: "ci-runner" });
    const scopeBoxes = [
      (() => { const box = checkbox(true, "* （全部）"); box.dataset.scope = "*"; return box; })(),
      ...agents.map((a) => {
        const box = checkbox(false, a.slug);
        box.dataset.scope = a.slug;
        return box;
      }),
    ];
    const doIssue = async (close) => {
      const scopes = scopeBoxes.filter((b) => b.checked).map((b) => b.dataset.scope);
      try {
        const data = await api.post("/agent-connectors", { name: name.value.trim(), scopes });
        close();
        showTokenOnce(data.name, data.token);
        rerender();
      } catch (err) { reportError(err); }
    };
    openModal(t("conn.issue"),
      h("div", { class: "form-grid" },
        field(t("conn.name"), name, { full: true }),
        field(t("conn.scopes"),
          h("div", { class: "dep-chips", style: { flexDirection: "column", alignItems: "flex-start" } }, ...scopeBoxes),
          { full: true })),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("conn.issue"), kind: "primary", onClick: doIssue },
      ]);
  };

  const showTokenOnce = (name, token) => {
    const copy = h("button", {
      class: "btn sm",
      onclick: async () => {
        try { await navigator.clipboard.writeText(token); toast(t("common.copied"), "ok"); }
        catch { /* 剪贴板不可用时手选复制 */ }
      },
    }, t("common.copy"));
    openModal(`${t("conn.issue")} · ${name}`,
      h("div", {},
        h("p", { class: "warn-box" }, t("conn.tokenOnce")),
        h("div", { class: "token-reveal mt" }, h("code", {}, token), copy)),
      [{ label: t("common.close"), kind: "primary", onClick: (close) => close() }]);
  };

  // -----------------------------------------------------------------------
  // 模块：团队（DAG 编辑器 v1）
  // -----------------------------------------------------------------------
  const renderTeams = async (container) => {
    const items = await api.get("/teams?include_disabled=true");
    const columns = [
      { label: t("team.id") }, { label: t("team.name") }, { label: t("team.steps") },
      { label: t("team.maxRuns") }, { label: t("common.enabled") },
      { label: t("common.revision") }, { label: t("common.actions") },
    ];
    const rows = (items || []).map((team) => h("tr", {},
      h("td", { class: "mono" }, team.id),
      h("td", {}, team.name, team.description ? h("div", { class: "muted small" }, team.description) : null),
      h("td", {}, dagPreview(team.steps || [])),
      h("td", {}, String(team.max_concurrent_runs ?? 4)),
      h("td", {}, team.enabled ? h("span", { class: "dot on" }) : h("span", { class: "dot off" })),
      h("td", { class: "muted" }, String(team.revision)),
      h("td", { class: "nowrap" },
        h("button", { class: "btn sm", onclick: () => teamEditor(team) }, t("common.edit")),
        " ",
        h("button", { class: "btn sm primary", onclick: () => submitRunModal(team) }, t("team.run.submit")),
        " ",
        h("button", { class: "btn sm", onclick: () => shareTeamModal(team) }, t("team.share")),
        " ",
        h("button", {
          class: "btn sm danger",
          onclick: () => confirmDelete(team.id, async () => {
            try {
              await api.del(`/teams/${encodeURIComponent(team.id)}`, { ifMatch: team.revision });
              toast(t("common.deleted"), "ok");
              rerender();
            } catch (err) { reportError(err); }
          }),
        }, t("common.delete")))));
    if (!rows.length) rows.push(emptyRow(columns.length));
    container.append(panel(null, null,
      h("div", { class: "row mb" },
        h("button", { class: "btn primary", onclick: () => teamEditor(null) }, `+ ${t("team.new")}`)),
      table(columns, rows)));
  };

  // 拓扑分层预览（v1：Kahn 分层，列间箭头；环则平铺展示）。
  const dagLevels = (steps) => {
    const byId = new Map(steps.map((s) => [s.id, s]));
    const levels = [];
    const placed = new Set();
    let remaining = [...steps];
    while (remaining.length) {
      const ready = remaining.filter((s) => (s.depends_on || []).every((d) => placed.has(d) || !byId.has(d)));
      if (!ready.length) { levels.push(remaining); break; } // 有环：整体放最后一层
      levels.push(ready);
      for (const s of ready) placed.add(s.id);
      remaining = remaining.filter((s) => !placed.has(s.id));
    }
    return levels;
  };

  const stepTargetLabel = (step) => {
    const target = step.target || {};
    if (target.agent_slug) return `🤖 ${target.agent_slug}`;
    if (target.position_code) return `◇ ${target.position_code}`;
    return "—";
  };

  const dagPreview = (steps) => {
    if (!steps.length) return h("span", { class: "muted" }, "—");
    const levels = dagLevels(steps);
    const wrap = h("div", { class: "dag" });
    levels.forEach((level, index) => {
      if (index > 0) wrap.append(h("div", { class: "dag-arrow" }, "→"));
      wrap.append(h("div", { class: "dag-level" },
        level.map((s) => h("div", { class: "dag-node", title: (s.depends_on || []).join(", ") },
          h("div", { class: "mono" }, s.id),
          h("div", { class: "t" }, stepTargetLabel(s))))));
    });
    return wrap;
  };

  const teamEditor = async (existing) => {
    const { positions, agents } = await loadCatalog();
    const id = textInput(existing ? existing.id : "", { placeholder: "research-pipeline" });
    if (existing) id.readOnly = true;
    const name = textInput(existing ? existing.name : "");
    const description = textInput(existing ? existing.description : "");
    const maxRuns = textInput(existing ? String(existing.max_concurrent_runs ?? 4) : "4", { type: "number", min: "1" });
    const enabled = checkbox(existing ? existing.enabled : true, t("common.enabled"));

    // 步骤草稿（编辑器内部形态；保存时映射回 team.Step JSON）。
    const draftSteps = (existing ? existing.steps || [] : []).map((s) => ({
      id: s.id,
      targetType: s.target && s.target.agent_slug ? "agent" : "position",
      targetValue: (s.target && (s.target.agent_slug || s.target.position_code)) || "",
      dependsOn: [...(s.depends_on || [])],
      inputTemplate: s.input_template ? prettyJSON(s.input_template) : "",
      onFailure: s.on_failure || "fail_fast",
      maxAttempts: (s.retry && s.retry.max_attempts) ?? 1,
      timeoutSecs: s.timeout_secs ?? null,
    }));

    const stepsWrap = h("div");
    const previewWrap = h("div", { class: "mt" });

    const buildStepsJSON = () => draftSteps.map((s) => {
      if (!s.id) throw new Error("step id is required");
      if (!s.targetValue) throw new Error(`step ${s.id}: 未选择目标`);
      const step = {
        id: s.id,
        target: s.targetType === "agent"
          ? { agent_slug: s.targetValue }
          : { position_code: s.targetValue },
        retry: { max_attempts: Number(s.maxAttempts) || 1 },
        on_failure: s.onFailure || "fail_fast",
      };
      if (s.dependsOn.length) step.depends_on = s.dependsOn;
      if (s.inputTemplate.trim()) step.input_template = JSON.parse(s.inputTemplate);
      if (s.timeoutSecs !== null && s.timeoutSecs !== undefined && s.timeoutSecs !== "") {
        step.timeout_secs = Number(s.timeoutSecs);
      }
      return step;
    });

    const refreshPreview = () => {
      clear(previewWrap);
      try {
        const steps = buildStepsJSON();
        previewWrap.append(h("div", { class: "muted small mb" }, t("team.preview")), dagPreview(steps));
      } catch (err) {
        previewWrap.append(h("div", { class: "err-text small" }, err.message));
      }
    };

    const renderStepEditors = () => {
      clear(stepsWrap);
      draftSteps.forEach((step, index) => {
        const idInput = textInput(step.id, { placeholder: "step-a" });
        idInput.addEventListener("input", () => { step.id = idInput.value.trim(); refreshPreview(); });
        const typeSelect = selectInput([
          { value: "position", label: t("team.step.target.position") },
          { value: "agent", label: t("team.step.target.agent") },
        ], step.targetType);
        // 空值占位选项保证「未选择」在视觉上显式（否则浏览器默认选中第一
        // 项而内部模型仍是空串，保存时才被后端拒绝）。
        const valueSelect = selectInput(
          [{ value: "", label: "— 选择 —" }].concat(
            step.targetType === "agent"
              ? agents.map((a) => ({ value: a.slug, label: a.slug }))
              : positions.map((p) => ({ value: p.code, label: `${p.code} · ${p.name}` }))),
          step.targetValue);
        typeSelect.addEventListener("change", () => {
          step.targetType = typeSelect.value;
          step.targetValue = "";
          renderStepEditors();
        });
        valueSelect.addEventListener("change", () => { step.targetValue = valueSelect.value; refreshPreview(); });
        const failureSelect = selectInput(["fail_fast", "continue", "skip_branch"]
          .map((m) => ({ value: m, label: t(`team.onfailure.${m}`) })), step.onFailure);
        failureSelect.addEventListener("change", () => { step.onFailure = failureSelect.value; refreshPreview(); });
        const retryInput = textInput(String(step.maxAttempts), { type: "number", min: "1" });
        retryInput.addEventListener("input", () => { step.maxAttempts = retryInput.value; refreshPreview(); });
        const timeoutInput = textInput(step.timeoutSecs === null ? "" : String(step.timeoutSecs), { type: "number", min: "1" });
        timeoutInput.addEventListener("input", () => {
          step.timeoutSecs = timeoutInput.value.trim() === "" ? null : timeoutInput.value;
          refreshPreview();
        });
        const templateArea = h("textarea", { rows: 3, placeholder: '{"topic": "{{team.input.topic}}"}' });
        templateArea.value = step.inputTemplate;
        templateArea.addEventListener("input", () => { step.inputTemplate = templateArea.value; refreshPreview(); });

        const depChips = h("div", { class: "dep-chips" });
        const renderDepChips = () => {
          clear(depChips);
          const others = draftSteps.filter((_, otherIndex) => otherIndex !== index);
          if (!others.length) {
            depChips.append(h("span", { class: "muted small" }, t("team.step.entry")));
            return;
          }
          for (const other of others) {
            const on = step.dependsOn.includes(other.id);
            const chip = h("span", {
              class: `chip dep-chip ${on ? "on" : ""}`,
              onclick: () => {
                if (on) step.dependsOn = step.dependsOn.filter((d) => d !== other.id);
                else step.dependsOn.push(other.id);
                renderDepChips();
                refreshPreview();
              },
            }, other.id || "…");
            depChips.append(chip);
          }
        };
        renderDepChips();

        stepsWrap.append(h("div", { class: "step-editor" },
          h("div", { class: "step-editor-head" },
            h("span", { class: "step-num" }, `#${index + 1}`),
            h("div", { class: "spacer" }),
            h("button", {
              class: "btn sm",
              onclick: () => {
                draftSteps.splice(index, 0, {
                  id: "", targetType: "position", targetValue: "", dependsOn: [],
                  inputTemplate: "", onFailure: "fail_fast", maxAttempts: 1, timeoutSecs: null,
                });
                renderStepEditors();
              },
            }, "↑＋"),
            h("button", {
              class: "btn sm danger",
              onclick: () => { draftSteps.splice(index, 1); renderStepEditors(); },
            }, "✕")),
          h("div", { class: "form-grid" },
            field(t("team.step.id"), idInput),
            field(t("team.step.onFailure"), failureSelect),
            field(t("team.step.target"), h("div", { class: "row" }, typeSelect, valueSelect), { full: true }),
            field(t("team.step.deps"), depChips, { full: true }),
            field(t("team.step.retry"), retryInput),
            field(t("team.step.timeout"), timeoutInput),
            field(t("team.step.inputTemplate"), templateArea, { full: true }))));
      });
      // 依赖 chips 需要在全部步骤渲染完成后再刷一遍（新增/删除步骤后 id 集合变化）。
      refreshPreview();
    };

    const addStep = () => {
      draftSteps.push({
        id: `step-${draftSteps.length + 1}`, targetType: "position", targetValue: "",
        dependsOn: [], inputTemplate: "", onFailure: "fail_fast", maxAttempts: 1, timeoutSecs: null,
      });
      renderStepEditors();
    };

    const save = async (close) => {
      let steps;
      try { steps = buildStepsJSON(); }
      catch (err) { toast(err.message, "err", 6000); return; }
      const body = {
        id: id.value.trim(), name: name.value.trim(), description: description.value.trim(),
        steps, max_concurrent_runs: Number(maxRuns.value) || 4, enabled: enabled.checked,
      };
      try {
        if (existing) {
          await api.put(`/teams/${encodeURIComponent(existing.id)}`, body, { ifMatch: existing.revision });
        } else {
          await api.post("/teams", body);
        }
        close();
        toast(t("common.saved"), "ok");
        rerender();
      } catch (err) { reportError(err); }
    };

    renderStepEditors();
    openModal(existing ? `${t("team.edit")} · ${existing.id}` : t("team.new"),
      h("div", {},
        h("div", { class: "form-grid mb" },
          field(t("team.id"), id),
          field(t("team.name"), name),
          field(t("pos.description"), description, { full: true }),
          field(t("team.maxRuns"), maxRuns),
          field(t("common.enabled"), enabled)),
        h("div", { class: "row mb" },
          h("h3", { class: "panel-title", style: { margin: 0 } }, t("team.editor")),
          h("div", { class: "spacer" }),
          h("button", { class: "btn sm", onclick: addStep }, `+ ${t("team.step.add")}`)),
        stepsWrap,
        previewWrap),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("common.save"), kind: "primary", onClick: save },
      ],
      { wide: true, dismissable: false });
  };

  // 分享对话框：生成/轮换公开链接（复制即用）+ 撤销。链接带一次性 token，
  // 页面在 /share/{token}，访客无需登录即可触发并查看交易信号。
  const shareTeamModal = async (team) => {
    const statusEl = h("div", { class: "small mt" });
    const linkBox = h("div", { class: "token-reveal mt" });
    const renderShare = (data) => {
      clear(linkBox);
      linkBox.append(h("code", {}, data.url),
        h("button", {
          class: "btn sm",
          onclick: async () => {
            try { await navigator.clipboard.writeText(data.url); toast(t("common.copied"), "ok"); }
            catch { /* 剪贴板不可用时手选 */ }
          },
        }, t("common.copy")));
    };
    const issue = async (rotate) => {
      clear(statusEl);
      try {
        const data = await api.post(`/teams/${encodeURIComponent(team.id)}/share`);
        renderShare(data);
        statusEl.append(h("div", { class: "ok-text" },
          rotate ? t("team.share.rotated") : t("team.share.done")));
      } catch (err) { reportError(err); }
    };
    openModal(`${t("team.share")} · ${team.name || team.id}`,
      h("div", {},
        h("p", { class: "muted small" }, t("team.share.hint")),
        linkBox, statusEl),
      [
        {
          label: t("team.share.revoke"), kind: "danger",
          onClick: async (close) => {
            try {
              await api.del(`/teams/${encodeURIComponent(team.id)}/share`);
              toast(t("team.share.revoked"), "ok");
              close();
            } catch (err) { reportError(err); }
          },
        },
        { label: t("common.close"), onClick: (close) => close() },
        { label: t("team.share.rotate"), kind: "primary", onClick: () => issue(true) },
      ]);
    await issue(false);
  };

  const submitRunModal = (team) => {
    const input = h("textarea", { rows: 5, placeholder: '{"topic": "…"}' });
    input.value = "{}";
    const idem = textInput("", { placeholder: "auto" });
    const submit = async (close) => {
      let parsed;
      try { parsed = JSON.parse(input.value || "{}"); }
      catch (err) { toast(`input: ${err.message}`, "err"); return; }
      const key = idem.value.trim() ||
        `console-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      try {
        const run = await api.post("/team-runs", {
          team_id: team.id, input: parsed, idempotency_key: key,
        });
        close();
        toast(`${t("team.run.submitted")} · ${run.id}`, "ok");
        navigate("/runs");
      } catch (err) { reportError(err); }
    };
    openModal(`${t("team.run.submit")} · ${team.name || team.id}`,
      h("div", {},
        field(t("team.run.input"), input, { full: true }),
        h("div", { class: "mt" }),
        field(t("team.run.idem"), idem, { full: true })),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("team.run.submit"), kind: "primary", onClick: submit },
      ]);
  };

  // -----------------------------------------------------------------------
  // 模块：运行日志（智能体运行 / 团队运行 / 通道事件）
  // -----------------------------------------------------------------------
  // 日志 = 运行日志 + 审计日志（管理员）合并页，子 Tab 记忆上次选择。
  const renderLogs = async (container) => {
    const items = [
      { key: "runs", label: t("nav.runs"), render: renderRuns },
    ];
    if (isAdmin()) items.push({ key: "audits", label: t("nav.audits"), render: renderAudits });
    const tabs = tabsEl(items, container.dataset.logsTab || "runs");
    container.append(tabs.el, tabs.body);
    const wanted = container.dataset.logsTab || "runs";
    tabs.activate(items.some((it) => it.key === wanted) ? wanted : "runs");
  };

  const renderRuns = async (container) => {
    const tabs = tabsEl([
      { key: "agent", label: t("run.tab.agent"), render: agentRunsTab },
      { key: "team", label: t("run.tab.team"), render: teamRunsTab },
      { key: "channel", label: t("run.tab.channel"), render: channelEventsTab },
    ], container.dataset.runsTab || "agent");
    container.append(tabs.el, tabs.body);
    tabs.activate(container.dataset.runsTab || "agent");
  };

  const RUN_STATUSES = ["", "queued", "leased", "running", "completed", "failed", "canceled"];

  const agentRunsTab = async (body) => {
    const filterBar = h("div", { class: "row mb" });
    const agentInput = textInput("", { placeholder: t("run.agent"), style: { width: "130px" } });
    const positionInput = textInput("", { placeholder: t("run.position"), style: { width: "140px" } });
    const statusSelect = selectInput(RUN_STATUSES.map((s) => ({ value: s, label: s || t("common.all") })), "");
    const errorCodeInput = textInput("", { placeholder: t("run.errorCode"), style: { width: "140px" } });
    const sinceInput = h("input", { type: "datetime-local", title: t("run.since") });
    const singleOnly = checkbox(false, t("run.singleOnly"));
    const apply = h("button", { class: "btn", onclick: () => load(1) }, t("common.filter"));
    filterBar.append(agentInput, positionInput, statusSelect, errorCodeInput, sinceInput, singleOnly, apply);

    const listWrap = h("div");
    body.append(panel(null, null, filterBar, listWrap));

    const columns = [
      { label: "" }, { label: t("run.runId") }, { label: t("run.agent") },
      { label: t("run.position") }, { label: t("run.status") }, { label: t("run.attempt") },
      { label: t("run.duration") }, { label: t("run.created") },
    ];

    const load = async (page) => {
      const params = new URLSearchParams({ limit: "30", offset: String((page - 1) * 30) });
      if (agentInput.value.trim()) params.set("agent", agentInput.value.trim());
      if (positionInput.value.trim()) params.set("position", positionInput.value.trim());
      if (statusSelect.value) params.set("status", statusSelect.value);
      if (errorCodeInput.value.trim()) params.set("error_code", errorCodeInput.value.trim());
      if (sinceInput.value) {
        const parsed = new Date(sinceInput.value);
        if (!Number.isNaN(parsed.getTime())) params.set("since", parsed.toISOString());
      }
      if (singleOnly.checked) params.set("single_only", "true");
      const data = await api.get(`/runs?${params}`);
      clear(listWrap);
      const rows = [];
      for (const run of data.items || []) {
        const detailId = `run-detail-${run.RunID}`;
        const row = h("tr", {
          class: "expandable",
          onclick: () => toggleRunDetail(row, run, detailId),
        },
          h("td", { class: "muted" }, "▸"),
          h("td", { class: "mono small" }, run.RunID,
            run.TeamRunID ? h("span", { class: "chip accent", title: run.TeamRunID }, "team") : null),
          h("td", {}, run.AgentSlug
            ? h("button", {
              class: "slug-btn mono", type: "button", title: t("agent.connect"),
              // 行本身可点击展开详情；标识按钮只负责弹接入框。
              onclick: (event) => {
                event.stopPropagation();
                connectModal({ slug: run.AgentSlug });
              },
            }, run.AgentSlug)
            : "—"),
          h("td", {}, run.PositionCode ? h("span", { class: "chip" }, run.PositionCode) : "—"),
          h("td", {}, statusChip(String(run.Status))),
          h("td", { class: "muted" }, String(run.Attempt ?? 1)),
          h("td", { class: "muted" }, fmtDuration(run.DurationMS)),
          h("td", { class: "muted small nowrap" }, fmtTime(run.CreatedAt)));
        rows.push(row);
      }
      if (!rows.length) rows.push(emptyRow(columns.length));
      listWrap.append(...[table(columns, rows), pager(data.total || 0, page, 30, load)].filter(Boolean));
    };
    await load(1);
  };

  const toggleRunDetail = async (row, run, detailId) => {
    const existing = document.getElementById(detailId);
    const arrow = row.firstChild;
    if (existing) { existing.remove(); arrow.textContent = "▸"; return; }
    arrow.textContent = "▾";
    // 列表是瘦列（无 Input/Output）：展开时按 runID 取详情。
    if (run.Input == null && run.Output == null) {
      try { run = await api.get(`/runs/${encodeURIComponent(run.RunID)}`); }
      catch { /* 详情失败时以列表行数据兜底展示 */ }
    }
    const cell = h("td", { colspan: 8 });
    const detail = h("div", {},
      h("div", { class: "kv mb" },
        h("span", { class: "k" }, t("run.status")), h("span", { class: "v" }, statusChip(String(run.Status))),
        h("span", { class: "k" }, t("agent.binding")),
        h("span", { class: "v" }, `${run.ExecutorKind || "—"} / ${run.ExecutionMode || "—"}`),
        run.TeamRunID ? h("span", { class: "k" }, t("run.teamRun")) : null,
        run.TeamRunID ? h("span", { class: "v mono" }, `${run.TeamRunID} · ${run.TeamStepID || ""}`) : null,
        run.ErrorCode ? h("span", { class: "k" }, t("run.errorCode")) : null,
        run.ErrorCode ? h("span", { class: "v err-text" }, run.ErrorCode) : null,
        run.Usage ? h("span", { class: "k" }, t("run.usage")) : null,
        run.Usage ? h("span", { class: "v mono" },
          `in ${run.Usage.inputTokens ?? 0} · out ${run.Usage.outputTokens ?? 0} · total ${run.Usage.totalTokens ?? 0}`) : null,
        h("span", { class: "k" }, t("run.completed")), h("span", { class: "v" }, fmtTime(run.CompletedAt))),
      h("div", { class: "muted small" }, t("run.input")), pre(run.Input),
      h("div", { class: "muted small" }, t("run.output")), pre(run.Output),
      run.ExecutorOutput ? frag(h("div", { class: "muted small" }, t("run.executorOutput")), pre(run.ExecutorOutput)) : null);
    cell.append(detail);
    // 团队子运行：附带同 TeamRun 的兄弟步骤运行，形成父子视图。
    if (run.TeamRunID) {
      try {
        const siblings = await api.get(`/runs?team_run_id=${encodeURIComponent(run.TeamRunID)}&limit=50`);
        cell.append(h("div", { class: "muted small mt" }, t("run.childRuns")),
          siblingTable(siblings.items || []));
      } catch { /* 子表失败不阻塞主详情 */ }
    }
    row.after(h("tr", { class: "detail-row", id: detailId }, cell));
  };

  const siblingTable = (items) => table(
    [{ label: t("run.step") }, { label: t("run.runId") }, { label: t("run.agent") },
      { label: t("run.status") }, { label: t("run.duration") }],
    items.map((r) => h("tr", {},
      h("td", { class: "mono" }, r.TeamStepID || "—"),
      h("td", { class: "mono small" }, r.RunID),
      h("td", { class: "mono" }, r.AgentSlug),
      h("td", {}, statusChip(String(r.Status))),
      h("td", { class: "muted" }, fmtDuration(r.DurationMS)))));

  const pager = (total, page, pageSize, load) => {
    const pages = Math.max(1, Math.ceil(total / pageSize));
    if (total <= 0) return null; // 空列表不渲染 total 0
    if (pages <= 1) return null; // 单页无翻页价值，total 不显示
    return h("div", { class: "row mt" },
      h("span", { class: "muted small" }, `total ${total} · ${page}/${pages}`),
      h("button", { class: "btn sm", disabled: page <= 1, onclick: () => load(page - 1) }, "←"),
      h("button", { class: "btn sm", disabled: page >= pages, onclick: () => load(page + 1) }, "→"));
  };

  // makeList：通用列表组件（B5）——筛选行 + 分页 + 表格一体。
  // 契约：fetch(params) 返回 {items, total}；controls[{el, param, value}]
  // 的输入变化即触发刷新；extras 为附加控件（如导出按钮）。
  const makeList = ({ columns, pageSize, controls = [], extras = [], fetch, renderRow }) => {
    const root = h("div");
    const controlsRow = h("div", { class: "row mb" });
    for (const c of controls) {
      if (c.el) {
        c.el.addEventListener("change", () => load(1));
        controlsRow.append(c.el);
      }
    }
    for (const extra of extras) controlsRow.append(extra);
    controlsRow.append(h("button", { class: "btn sm", onclick: () => load(1) }, t("common.filter")));
    const listWrap = h("div");
    root.append(controlsRow, listWrap);
    const load = async (page = 1) => {
      const params = new URLSearchParams();
      params.set("page", String(page));
      params.set("pageSize", String(pageSize));
      for (const c of controls) {
        const value = (c.value ? c.value() : "").trim();
        if (value) params.set(c.param, value);
      }
      try {
        const data = await fetch(params.toString());
        const rows = (data.items || []).map(renderRow);
        if (!rows.length) rows.push(emptyRow(columns.length));
        listWrap.replaceChildren(table(columns, rows), pager(data.total || 0, page, pageSize, load) || h("div"));
      } catch (err) {
        reportError(err);
      }
    };
    return { root, load };
  };

  const teamRunsTab = async (body) => {
    const listWrap = h("div");
    body.append(panel(null, null, listWrap));
    const columns = [
      { label: "" }, { label: t("run.runId") }, { label: t("run.team") },
      { label: t("run.status") }, { label: t("run.created") },
      { label: t("run.completed") }, { label: t("common.actions") },
    ];
    const load = async (page) => {
      const data = await api.get(`/team-runs?limit=30&offset=${(page - 1) * 30}`);
      clear(listWrap);
      const rows = [];
      for (const tr of data.items || []) {
        const detailId = `team-run-${tr.id}`;
        const row = h("tr", {
          class: "expandable",
          onclick: (event) => {
            if (event.target.closest("button")) return;
            toggleTeamRunDetail(row, tr, detailId);
          },
        },
          h("td", { class: "muted" }, "▸"),
          h("td", { class: "mono small" }, tr.id),
          h("td", { class: "mono" }, tr.team_id),
          h("td", {}, statusChip(tr.status)),
          h("td", { class: "muted small nowrap" }, fmtTime(tr.created_at)),
          h("td", { class: "muted small nowrap" }, fmtTime(tr.completed_at)),
          h("td", { class: "nowrap" },
            isActiveTeamRun(tr.status)
              ? h("button", {
                class: "btn sm danger",
                onclick: async () => {
                  try {
                    await api.post(`/team-runs/${encodeURIComponent(tr.id)}/cancel`);
                    toast(t("run.canceled"), "ok");
                    load(page);
                  } catch (err) { reportError(err); }
                },
              }, t("run.cancel"))
              : tr.status === "failed"
                ? h("button", {
                  class: "btn sm",
                  title: t("run.retry"),
                  onclick: async () => {
                    try {
                      const retried = await api.post(`/team-runs/${encodeURIComponent(tr.id)}/retry`);
                      toast(`${t("run.retry.done")} · ${retried.id}`, "ok");
                      load(page);
                    } catch (err) { reportError(err); }
                  },
                }, t("run.retry"))
                : null));
        rows.push(row);
      }
      if (!rows.length) rows.push(emptyRow(columns.length));
      listWrap.append(...[table(columns, rows), pager(data.total || 0, page, 30, load)].filter(Boolean));
    };
    await load(1);
  };

  const isActiveTeamRun = (status) => status === "queued" || status === "running";

  const toggleTeamRunDetail = async (row, tr, detailId) => {
    const existing = document.getElementById(detailId);
    const arrow = row.firstChild;
    if (existing) { existing.remove(); arrow.textContent = "▸"; return; }
    arrow.textContent = "▾";
    let full = tr;
    try { full = await api.get(`/team-runs/${encodeURIComponent(tr.id)}`); } catch { /* 用列表行数据兜底 */ }
    const steps = full.steps || {};
    const stepRows = Object.values(steps).map((s) => h("tr", {},
      h("td", { class: "mono" }, s.step_id),
      h("td", {}, statusChip(s.state)),
      h("td", { class: "muted" }, String(s.attempts ?? 0)),
      h("td", { class: "mono small" }, s.child_run_id || "—"),
      h("td", {}, s.error ? h("span", { class: "err-text" }, `${s.error.code || ""} ${s.error.message || ""}`) : "—"),
      h("td", { class: "muted small nowrap" }, s.started_at ? fmtTime(s.started_at) : "—"),
      h("td", { class: "muted small nowrap" }, s.ended_at ? fmtTime(s.ended_at) : "—")));
    const cell = h("td", { colspan: 7 },
      h("div", { class: "muted small mb" }, t("run.stepsState")),
      stepRows.length
        ? table([{ label: t("run.state.step_id") }, { label: t("run.status") },
          { label: t("run.attempt") }, { label: t("run.childRuns") },
          { label: t("run.errorCode") }, { label: "started" }, { label: "ended" }], stepRows)
        : h("div", { class: "empty" }, t("common.empty")),
      full.error ? h("div", { class: "err-text mt" }, `${full.error.code || ""} ${full.error.message || ""}`) : null);
    row.after(h("tr", { class: "detail-row", id: detailId }, cell));
  };

  const channelEventsTab = async (body) => {
    const slugInput = textInput("", { placeholder: t("run.agent"), style: { width: "150px" } });
    const typeInput = textInput("", { placeholder: t("run.channel.type"), style: { width: "130px" } });
    const listWrap = h("div");
    const apply = h("button", { class: "btn", onclick: () => load() }, t("common.filter"));
    body.append(panel(null, null,
      h("div", { class: "row mb" }, typeInput, slugInput, apply), listWrap));
    const columns = [
      { label: t("run.channel.type") }, { label: t("run.agent") }, { label: t("run.runId") },
      { label: t("run.channel.url") }, { label: t("run.channel.status") },
      { label: t("run.channel.accepted") }, { label: t("run.channel.pass") },
      { label: t("run.channel.latency") }, { label: t("run.created") },
    ];
    const load = async () => {
      const params = new URLSearchParams({ limit: "50" });
      if (slugInput.value.trim()) params.set("slug", slugInput.value.trim());
      if (typeInput.value.trim()) params.set("event_type", typeInput.value.trim());
      const items = await api.get(`/channel-events?${params}`);
      clear(listWrap);
      const rows = (items || []).map((e) => h("tr", { class: "expandable" },
        h("td", {}, h("span", { class: "chip accent" }, e.EventType)),
        h("td", { class: "mono" }, e.AgentSlug || "—"),
        h("td", { class: "mono small" }, e.RunID || "—"),
        h("td", { class: "mono small muted" }, e.URL || "—"),
        h("td", {}, e.StatusCode != null ? String(e.StatusCode) : "—"),
        h("td", {}, boolMark(e.Accepted)),
        h("td", {}, boolMark(e.Pass)),
        h("td", { class: "muted" }, e.LatencyMs != null ? `${e.LatencyMs}ms` : "—"),
        h("td", { class: "muted small nowrap" }, fmtTime(e.CreatedAt))));
      if (!rows.length) rows.push(emptyRow(columns.length));
      listWrap.append(table(columns, rows));
    };
    await load();
  };

  const boolMark = (value) => value === null || value === undefined
    ? h("span", { class: "muted" }, "—")
    : value ? h("span", { class: "ok-text" }, "✓") : h("span", { class: "err-text" }, "✗");

  // -----------------------------------------------------------------------
  // 模块：审计日志（admin only；后端强制 403）
  // -----------------------------------------------------------------------
  const renderAudits = async (container) => {
    if (!isAdmin()) {
      container.append(h("div", { class: "empty" }, "403"));
      return;
    }
    const actorInput = textInput("", { placeholder: t("audit.actor"), style: { width: "150px" } });
    const actionInput = textInput("", { placeholder: t("audit.action"), style: { width: "180px" } });
    const csvBtn = h("button", {
      class: "btn",
      onclick: () => {
        const params = new URLSearchParams({ limit: "10000" });
        if (actorInput.value.trim()) params.set("actor", actorInput.value.trim());
        if (actionInput.value.trim()) params.set("action", actionInput.value.trim());
        fetch(`${API_BASE}/audits/export?${params}`, { credentials: "same-origin" })
          .then((response) => {
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            return response.blob();
          })
          .then((blob) => {
            const url = URL.createObjectURL(blob);
            const link = h("a", { href: url, download: "audit-export.csv" });
            link.click();
            URL.revokeObjectURL(url);
          })
          .catch((err) => toast(err.message, "err"));
      },
    }, "⬇ CSV");
    const columns = [
      { label: t("audit.time") }, { label: t("audit.actor") }, { label: t("audit.action") },
      { label: t("audit.target") }, { label: t("audit.detail") },
    ];
    // B5：走通用列表组件（筛选行+分页+表格一体）。
    const list = makeList({
      columns, pageSize: 100,
      controls: [
        { el: actorInput, param: "actor", value: () => actorInput.value.trim() },
        { el: actionInput, param: "action", value: () => actionInput.value.trim() },
      ],
      extras: [csvBtn],
      fetch: (params) => api.get(`/audits?${params}`),
      renderRow: (e) => h("tr", {},
        h("td", { class: "muted small nowrap" }, fmtTime(e.CreatedAt)),
        h("td", { class: "mono" }, e.Actor),
        h("td", {}, h("span", { class: "chip accent" }, String(e.Action))),
        h("td", { class: "mono small" }, e.Target || "—"),
        h("td", { class: "mono small muted", style: { maxWidth: "380px", wordBreak: "break-all" } },
          e.Detail ? prettyJSON(e.Detail) : "—")),
    });
    container.append(panel(null, null, list.root));
    await list.load(1);
  };

  // -----------------------------------------------------------------------
  // 模块：账号管理（admin only）
  // -----------------------------------------------------------------------
  const renderUsers = async (container) => {
    if (!isAdmin()) {
      container.append(h("div", { class: "empty" }, "403"));
      return;
    }
    const items = await api.get("/users");
    const columns = [
      { label: t("user.username") }, { label: t("user.role") }, { label: t("common.enabled") },
      { label: t("user.created") }, { label: t("user.updated") }, { label: t("common.actions") },
    ];
    const rows = (items || []).map((u) => h("tr", {},
      h("td", { class: "mono" }, u.username),
      h("td", {}, h("span", { class: `chip ${u.role === "admin" ? "accent" : ""}` },
        t(`user.role.${u.role}`))),
      h("td", {}, u.enabled ? h("span", { class: "dot on" }) : h("span", { class: "dot off" })),
      h("td", { class: "muted small" }, fmtTime(u.created_at)),
      h("td", { class: "muted small" }, fmtTime(u.updated_at)),
      h("td", { class: "nowrap" },
        h("button", { class: "btn sm", onclick: () => userEditor(u) }, t("common.edit")),
        " ",
        h("button", {
          class: "btn sm danger",
          onclick: () => confirmDelete(u.username, async () => {
            try {
              await api.del(`/users/${encodeURIComponent(u.username)}`);
              toast(t("common.deleted"), "ok");
              rerender();
            } catch (err) { reportError(err); }
          }),
        }, t("common.delete")))));
    if (!rows.length) rows.push(emptyRow(columns.length));
    container.append(panel(t("user.title"), null,
      h("div", { class: "row mb" },
        h("button", { class: "btn primary", onclick: () => userEditor(null) }, `+ ${t("user.new")}`)),
      table(columns, rows)));
  };

  const userEditor = (existing) => {
    const username = textInput(existing ? existing.username : "", { placeholder: "ops-alice" });
    if (existing) username.readOnly = true;
    const role = selectInput([
      { value: "user", label: t("user.role.user") },
      { value: "admin", label: t("user.role.admin") },
    ], existing ? existing.role : "user");
    const password = h("input", {
      type: "password", autocomplete: "new-password",
      placeholder: existing ? t("user.password.keep") : t("user.password.new"),
    });
    const enabled = checkbox(existing ? existing.enabled : true, t("common.enabled"));
    const save = async (close) => {
      try {
        if (existing) {
          const body = { role: role.value, enabled: enabled.checked };
          if (password.value) body.password = password.value;
          await api.put(`/users/${encodeURIComponent(existing.username)}`, body);
        } else {
          await api.post("/users", {
            username: username.value.trim(), password: password.value, role: role.value,
          });
        }
        close();
        toast(t("common.saved"), "ok");
        rerender();
      } catch (err) { reportError(err); }
    };
    openModal(existing ? `${t("user.edit")} · ${existing.username}` : t("user.new"),
      h("div", { class: "form-grid" },
        field(t("user.username"), username),
        field(t("user.role"), role),
        field(t("user.password"), password, { full: true }),
        field(t("common.enabled"), enabled)),
      [
        { label: t("common.cancel"), onClick: (close) => close() },
        { label: t("common.save"), kind: "primary", onClick: save },
      ]);
  };

  // -----------------------------------------------------------------------
  // 模块：设置（个人 / 系统）
  // -----------------------------------------------------------------------
  const renderSettings = async (container) => {
    const tabs = tabsEl([
      { key: "personal", label: t("set.tab.personal"), render: settingsPersonalTab },
      { key: "system", label: t("set.tab.system"), render: settingsSystemTab },
    ], container.dataset.settingsTab || "personal");
    container.append(tabs.el, tabs.body);
    tabs.activate(container.dataset.settingsTab || "personal");
  };

  const settingsPersonalTab = async (body) => {
    const themeGrid = h("div", { class: "theme-grid" });
    const themeNames = { gold: "set.theme.gold", copper: "set.theme.copper", silver: "set.theme.silver", diamond: "set.theme.diamond" };
    for (const theme of THEMES) {
      const card = h("div", {
        class: `theme-card ${prefs.theme === theme ? "selected" : ""}`,
        onclick: () => {
          prefs.theme = theme;
          for (const child of themeGrid.children) child.classList.remove("selected");
          card.classList.add("selected");
        },
      },
        h("div", { class: `theme-swatch ${theme}` }),
        h("div", { class: "theme-name" }, t(themeNames[theme])),
        theme === "gold" ? h("div", { class: "muted small" }, "default") : null);
      themeGrid.append(card);
    }
    const langSelect = selectInput([
      { value: "zh", label: "简体中文" },
      { value: "en", label: "English" },
    ], prefs.lang);
    langSelect.addEventListener("change", () => {
      prefs.lang = langSelect.value;
      renderApp(); // 全量重渲染以切换语言
    });
    body.append(panel(t("set.theme"), null, themeGrid),
      panel(t("set.lang"), null, h("div", { class: "row" }, langSelect)));
  };

  const settingsSystemTab = async (body) => {
    const snap = await api.get("/dashboard");
    body.append(panel(t("set.sys.info"), null,
      h("div", { class: "kv" },
        h("span", { class: "k" }, t("dash.version")), h("span", { class: "v mono" }, snap.version || "—"),
        h("span", { class: "k" }, t("dash.database")),
        h("span", { class: "v" }, snap.databaseOk
          ? h("span", { class: "ok-text" }, t("dash.dbOk"))
          : h("span", { class: "err-text" }, t("dash.dbDown"))),
        h("span", { class: "k" }, t("dash.serverTime")), h("span", { class: "v mono" }, fmtTime(snap.serverTime)),
        h("span", { class: "k" }, t("dash.onlineWorkers")), h("span", { class: "v" }, String(snap.onlineWorkers ?? 0)))));
  };

  // -----------------------------------------------------------------------
  // 路由与应用壳
  // -----------------------------------------------------------------------
  const ROUTES = {
    "/dashboard": renderDashboard,
    // 岗位已并入智能体模块：直链回落到智能体页并预选岗位子页签。
    "/positions": async (container) => {
      container.dataset.agentsTab = "positions";
      await renderAgents(container);
    },
    "/agents": renderAgents,
    "/teams": renderTeams,
    "/logs": renderLogs,
    // 旧链接兼容：直开对应子 Tab。
    "/runs": async (container) => {
      container.dataset.logsTab = "runs";
      await renderLogs(container);
    },
    "/audits": async (container) => {
      container.dataset.logsTab = "audits";
      await renderLogs(container);
    },
    "/users": renderUsers,
    "/settings": renderSettings,
  };

  let rerenderToken = 0;

  const rerender = () => renderRoute();

  const renderRoute = async () => {
    const content = document.getElementById("content");
    if (!content) return;
    const token = ++rerenderToken;
    let route = state.route;
    if (!ROUTES[route]) route = "/dashboard";
    const navItem = NAV.find((n) => n.route === route);
    if (navItem && navItem.adminOnly && !isAdmin()) route = "/dashboard";
    document.getElementById("topbar-title").textContent = t(MODULE_TITLES[route] || "app.title");
    for (const link of document.querySelectorAll(".nav-item")) {
      link.classList.toggle("active", link.dataset.route === route);
    }
    clear(content);
    content.append(h("div", { class: "muted" }, t("common.loading")));
    try {
      const view = h("div");
      clear(content);
      content.append(view);
      await ROUTES[route](view);
    } catch (err) {
      if (token !== rerenderToken) return;
      clear(content);
      if (err instanceof ApiError && err.status === 401) {
        state.session = null;
        renderApp();
        return;
      }
      content.append(h("div", { class: "empty err-text" }, err.message || String(err)));
    }
  };

  const updateBadges = () => {
    for (const item of document.querySelectorAll(".nav-item")) {
      const nav = NAV.find((n) => n.route === item.dataset.route);
      if (!nav || !nav.badge) continue;
      const value = nav.badge(state.badges) || 0;
      const badge = item.querySelector(".nav-badge");
      if (value > 0) {
        if (badge) badge.textContent = String(value);
        else item.append(h("span", { class: "nav-badge" }, String(value)));
      } else if (badge) badge.remove();
    }
  };

  const refreshBadges = async () => {
    if (!state.session) return;
    try {
      const snap = await api.get("/dashboard");
      state.badges = {
        activeTeamRuns: snap.activeTeamRuns || 0,
        runsToday: sumTotals(snap.runsToday),
        onlineWorkers: snap.onlineWorkers || 0,
      };
      updateBadges();
    } catch { /* 角标刷新失败静默 */ }
  };

  const renderShell = (root) => {
    const sidebar = h("aside", { class: `sidebar ${prefs.collapsed ? "collapsed" : ""}`, id: "sidebar" });
    const nav = h("nav", { class: "nav" });
    const groups = [
      { label: t("nav.group.business"), key: "business" },
      { label: t("nav.group.system"), key: "system" },
    ];
    for (const group of groups) {
      for (const item of NAV.filter((n) => n.group === group.key)) {
        if (item.adminOnly && !isAdmin()) continue;
        nav.append(h("a", {
          class: "nav-item", href: `#${item.route}`, dataset: { route: item.route },
          title: t(item.key),
          onclick: (event) => {
            event.preventDefault();
            state.route = item.route;
            navigate(item.route);
          },
        },
          h("span", { class: "nav-icon" }, item.icon),
          h("span", { class: "nav-label" }, t(item.key))));
      }
    }
    const collapseBtn = h("button", {
      class: "collapse-btn",
      title: prefs.collapsed ? t("nav.expand") : t("nav.collapse"),
      onclick: () => {
        prefs.collapsed = !prefs.collapsed;
        renderApp();
      },
    }, prefs.collapsed ? "»" : "«");
    sidebar.append(
      h("div", { class: "brand" },
        h("div", { class: "brand-name" }, t("app.short"))),
      nav,
      // 当前用户固定在侧栏左下角（折叠时隐藏文本块）。
      h("div", { class: "sidebar-user" },
        h("span", { class: "chip accent" }, t(`user.role.${state.session.role}`)),
        h("span", { class: "mono sidebar-user-name", title: state.session.username }, state.session.username)),
      h("div", { class: "sidebar-footer" },
        collapseBtn,
        h("div", { class: "spacer" }),
        h("button", {
          class: "btn sm",
          onclick: async () => {
            try { await api.post("/logout"); } catch { /* 会话可能已过期 */ }
            api.csrf = "";
            state.session = null;
            navigate("/dashboard");
            renderApp();
          },
        }, t("user.logout"))));

    const topbar = h("header", { class: "topbar" },
      h("div", { class: "topbar-title", id: "topbar-title" }, ""));

    const content = h("main", { class: "content", id: "content" });
    root.append(h("div", { class: "shell" }, sidebar,
      h("div", { class: "main" }, topbar, content)));
    renderRoute();
    refreshBadges();
  };

  const renderApp = () => {
    applyTheme();
    document.title = t("app.title");
    const root = clear(document.getElementById("app"));
    if (!state.session) {
      renderLogin(root);
      return;
    }
    renderShell(root);
  };

  // -----------------------------------------------------------------------
  // 启动
  // -----------------------------------------------------------------------
  window.addEventListener("hashchange", () => {
    state.route = location.hash.slice(1) || "/dashboard";
    renderRoute();
  });

  const boot = async () => {
    applyTheme();
    document.title = t("app.title");
    try {
      const session = await api.get("/session");
      api.csrf = session.csrfToken || "";
      state.session = { username: session.username, role: session.role };
    } catch {
      state.session = null;
    }
    renderApp();
    setInterval(refreshBadges, 30000);
  };

  boot();
})();
