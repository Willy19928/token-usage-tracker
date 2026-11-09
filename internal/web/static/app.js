const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const i18n = window.TokenUsageI18n;
const t = (key, values) => i18n.t(key, values);
const serverText = (value) => i18n.translateServer(value);

const FILTER_FIELDS = {
  mode: { selector: "#filterMode", labelKey: "filter.mode" },
  date: { selector: "#filterDate", labelKey: "filter.date" },
  model: { selector: "#filterModel", labelKey: "filter.model" },
  source: { selector: "#filterSource", labelKey: "filter.source" },
  agent_type: { selector: "#filterAgent", labelKey: "filter.agent" },
  project: { selector: "#filterProject", labelKey: "filter.project" },
  confidence: { selector: "#filterConfidence", labelKey: "filter.confidence" }
};
const FILTER_LABEL_KEYS = { session_id: "filter.session" };
const fieldLabel = (key) => t(FILTER_FIELDS[key]?.labelKey || FILTER_LABEL_KEYS[key] || `dimension.${key}`);
const dimensionLabel = (key) => t(`dimension.${key}`);
const rangeLabel = (key) => t(`range.${key}Long`);
const DISPLAY_PREFERENCES_KEY = "token-usage-display-preferences";
const PROVIDER_KEY = "token-usage-provider";
const PROVIDERS = ["all", "claude", "codex", "antigravity"];
const DISPLAY_PREFERENCE_DEFAULTS = Object.freeze({
  fontSize: "comfortable",
  density: "balanced",
  theme: "system",
  motion: "system"
});
const DISPLAY_PREFERENCE_VALUES = Object.freeze({
  fontSize: ["compact", "comfortable", "large"],
  density: ["compact", "balanced", "relaxed"],
  theme: ["system", "light", "dark"],
  motion: ["system", "reduce"]
});

function readStorage(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

function writeStorage(key, value) {
  try { localStorage.setItem(key, value); } catch {}
}

function removeStorage(key) {
  try { localStorage.removeItem(key); } catch {}
}

function normalizeDisplayPreferences(value = {}) {
  const normalized = { ...DISPLAY_PREFERENCE_DEFAULTS };
  for (const [key, allowed] of Object.entries(DISPLAY_PREFERENCE_VALUES)) {
    if (allowed.includes(value?.[key])) normalized[key] = value[key];
  }
  return normalized;
}

function loadDisplayPreferences() {
  let saved = {};
  try { saved = JSON.parse(readStorage(DISPLAY_PREFERENCES_KEY) || "{}"); } catch {}
  if (!saved || typeof saved !== "object" || Array.isArray(saved)) saved = {};
  const legacyTheme = readStorage("token-usage-theme");
  if (!DISPLAY_PREFERENCE_VALUES.theme.includes(saved?.theme) && ["light", "dark"].includes(legacyTheme)) saved.theme = legacyTheme;
  return normalizeDisplayPreferences(saved);
}

let displayPreferences = loadDisplayPreferences();

function syncSettingsForm() {
  $$('[data-setting]').forEach((input) => {
    const key = input.dataset.setting;
    const value = key === "locale" ? i18n.getLocale() : displayPreferences[key];
    input.checked = input.value === value;
  });
}

function applyDisplayPreferences({ persist = false } = {}) {
  const root = document.documentElement;
  root.dataset.fontSize = displayPreferences.fontSize;
  root.dataset.density = displayPreferences.density;
  if (displayPreferences.theme === "system") root.removeAttribute("data-theme");
  else root.dataset.theme = displayPreferences.theme;
  if (displayPreferences.motion === "system") root.removeAttribute("data-motion");
  else root.dataset.motion = displayPreferences.motion;

  if (persist) {
    writeStorage(DISPLAY_PREFERENCES_KEY, JSON.stringify(displayPreferences));
    if (displayPreferences.theme === "system") removeStorage("token-usage-theme");
    else writeStorage("token-usage-theme", displayPreferences.theme);
  }
  syncSettingsForm();
}

function updateDisplayPreference(key, value) {
  if (!DISPLAY_PREFERENCE_VALUES[key]?.includes(value)) return;
  displayPreferences = normalizeDisplayPreferences({ ...displayPreferences, [key]: value });
  applyDisplayPreferences({ persist: true });
}

function resetDisplayPreferences() {
  displayPreferences = { ...DISPLAY_PREFERENCE_DEFAULTS };
  removeStorage(DISPLAY_PREFERENCES_KEY);
  removeStorage("token-usage-theme");
  applyDisplayPreferences();
  toast(t("settings.resetComplete"));
}

const state = {
  provider: "all",
  view: "overview",
  viewVisited: { overview: true, daily: false, details: false, limits: false },
  overviewRange: "7d",
  customRange: null,
  customRangeLoading: false,
  customRangeError: "",
  detailRange: "30d",
  detailDimension: "model",
  filters: {},
  status: null,
  overview: null,
  hourlyLoaded: false,
  hourlyDate: "",
  hourlyNavigationDirection: 0,
  hourlyPoints: [],
  hourlyPointDate: "",
  pulsePoints: [],
  pulseDate: "",
  monthCursor: new Date(new Date().getFullYear(), new Date().getMonth(), 1),
  selectedDate: "",
  monthReport: null,
  dailyInitialSelection: true,
  pricing: null,
  pricingProvider: "codex",
  filterOptions: null,
  dataRevision: null,
  pendingDataRefresh: false,
  statusQualityNotes: [],
  dataQualityNotes: [],
  sessionSearch: "",
  sessionView: "list", treeOffset: 0, treeItems: [], treeCollapsed: new Set(),
  limitsDays: 7,
  limitsCustom: null,
  limitsHiddenSeries: new Set(),
  limitsShowUnknown: false,
  limitsKind: "fiveHour",
  limitsOverview: null,
  limitsAccount: null,
  limitsHistoryLimit: {},
  limits: null,
  requestSerial: { overview: 0, hourly: 0, hourlyContext: 0, daily: 0, day: 0, breakdown: 0, sessions: 0, estimates: 0, limits: 0 }
};

const responseCache = new Map();
const inflightRequests = new Map();
const DATA_CACHE_TTL = 5 * 60_000;
const VISIBLE_STATUS_POLL_MS = 30_000;
const HIDDEN_STATUS_POLL_MS = 10 * 60_000;
const LIMITS_REFRESH_MS = 60_000;
const LIMITS_HISTORY_PAGE = 30;
const LIMIT_PROVIDERS = ["codex", "claude", "antigravity"];
let cacheGeneration = 0;
let statusPollTimer = null;
let statusPollRunning = false;
let sessionSearchTimer = null;
let hourlyContextTimer = null;
let limitsRefreshTimer = null;

const reducedMotion = () => displayPreferences.motion === "reduce" || window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const pad2 = (value) => String(value).padStart(2, "0");
const dateKey = (date) => `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;
const dateFromKey = (value) => {
  const [year, month, day] = String(value).split("-").map(Number);
  return new Date(year, month - 1, day);
};
const addDays = (value, amount) => {
  const date = typeof value === "string" ? dateFromKey(value) : new Date(value);
  date.setDate(date.getDate() + amount);
  return date;
};
let measurementTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
const measuredDateKey = (date) => { const p=Object.fromEntries(new Intl.DateTimeFormat("en-CA", { timeZone: measurementTimezone, year:"numeric",month:"2-digit",day:"2-digit" }).formatToParts(date).map(x=>[x.type,x.value]));return `${p.year}-${p.month}-${p.day}`; };
const todayKey = () => measuredDateKey(new Date());
const measuredMinute = (date) => {
  const parts = Object.fromEntries(new Intl.DateTimeFormat("en-CA", {
    timeZone: measurementTimezone, year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", hourCycle: "h23"
  }).formatToParts(date).map((part) => [part.type, part.value]));
  return `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}`;
};
const emptyUsage = () => ({ input: 0, cached_input: 0, cache_write_input: 0, output: 0, reasoning_output: 0, total: 0 });
const usageTotal = (usage = {}) => Number(usage.total || (Number(usage.input || 0) + Number(usage.output || 0)) || 0);
const hourKey = (date) => `${dateKey(date)}T${pad2(date.getHours())}`;

function latestCompleteHour(now = new Date()) {
  const parts = Object.fromEntries(new Intl.DateTimeFormat("en",{timeZone:measurementTimezone,minute:"2-digit",second:"2-digit"}).formatToParts(now).map(p=>[p.type,p.value]));
  return new Date(now.getTime() - Number(parts.minute)*60000 - Number(parts.second)*1000 - now.getMilliseconds() - 3600000);
}

function normalizeHourlyDate(value, now = new Date()) {
  const fallback = measuredDateKey(latestCompleteHour(now));
  if (!/^\d{4}-\d{2}-\d{2}$/.test(String(value || ""))) return fallback;
  const parsed = dateFromKey(value);
  if (Number.isNaN(parsed.getTime()) || dateKey(parsed) !== value || value > measuredDateKey(now)) return fallback;
  return value;
}

function hourlyWindow(selectedDate = state.hourlyDate, now = new Date()) {
  const date = normalizeHourlyDate(selectedDate, now);
  const chartStart = dateFromKey(date);
  const dayEnd = addDays(chartStart, 1);
  const currentHour = new Date(now);
  currentHour.setMinutes(0, 0, 0);
  const chartEnd = date === dateKey(now) && currentHour < dayEnd ? currentHour : dayEnd;
  return {
    date,
    chartStart,
    chartEnd,
    completeHours: Math.max(0, Math.round((chartEnd.getTime() - chartStart.getTime()) / 60 / 60_000))
  };
}

function formatClock(value) {
  return value.toLocaleTimeString(i18n.getLocale(), { hour: "2-digit", minute: "2-digit", hour12: false, timeZone: measurementTimezone });
}

function formatHourWindow(start, end) {
  const dateTime = (value) => `${i18n.formatDate(value, { month: "short", day: "numeric", timeZone: measurementTimezone })} ${formatClock(value)}`;
  return `${dateTime(start)}–${measuredDateKey(start) === measuredDateKey(end) ? formatClock(end) : dateTime(end)}`;
}

function fillCompleteHours(rawPoints, windows) {
  const byHour = new Map((rawPoints || []).map((point) => [new Date(point.time).getTime(), point]));
  return Array.from({ length: windows.completeHours }, (_, index) => {
    const start = new Date(windows.chartStart.getTime() + index * 60 * 60_000);
    const existing = byHour.get(start.getTime());
    return { date: start.toISOString(), start, usage: existing?.usage || emptyUsage(), modes: existing?.modes };
  });
}

function rangeBounds(range) {
  if (range === "custom") {
    if (state.filters.date) return {};
    const { since, until } = state.customRange;
    return { since, until, label: `${since.replace("T", " ")} → ${until.replace("T", " ")}` };
  }
  if (range === "all") return { label: rangeLabel("all") };
  const today = dateFromKey(todayKey());
  const days = range === "today" ? 1 : range === "7d" ? 7 : 30;
  return {
    since: dateKey(addDays(today, -(days - 1))),
    until: dateKey(addDays(today, 1)),
    label: rangeLabel(range)
  };
}

function syncCustomRangeControls() {
  const active = state.overviewRange === "custom" && !state.filters.date;
  $("#customRangeForm").hidden = !active;
  $("#overviewExactTotal").hidden = !active;
  $("#customRangeHint").textContent = t("range.hint", { zone: measurementTimezone });
  const pending = state.customRange && ($("#rangeStart").value !== state.customRange.since || $("#rangeEnd").value !== state.customRange.until);
  const status = $("#customRangeStatus");
  status.textContent = pending ? t("range.pending") : state.customRangeLoading ? t("range.querying") : "";
  status.hidden = !status.textContent;
  $("#queryRange").disabled = state.customRangeLoading;
  $("#customRangeError").textContent = state.customRangeError;
  $("#customRangeError").hidden = !state.customRangeError;
}

function openCustomRange() {
  if (!state.customRange) {
    const now = new Date();
    state.customRange = { since: measuredMinute(new Date(now.getTime() - 60 * 60_000)), until: measuredMinute(now) };
    if (state.customRange.since >= state.customRange.until) {
      state.customRange.since = measuredMinute(new Date(now.getTime() - 24 * 60 * 60_000));
    }
  }
  $("#rangeStart").value = state.customRange.since;
  $("#rangeEnd").value = state.customRange.until;
  state.customRangeError = "";
}

function queryCustomRange(event) {
  event.preventDefault();
  if (!$("#customRangeForm").reportValidity()) return;
  const since = $("#rangeStart").value;
  const until = $("#rangeEnd").value;
  if (since >= until) {
    state.customRangeError = t("range.orderError");
    syncCustomRangeControls();
    $("#rangeEnd").focus();
    return;
  }
  state.customRange = { since, until };
  state.customRangeError = "";
  state.pulseDate = "";
  loadOverview();
}

function syncRangeControls() {
  syncCustomRangeControls();
  const selectedDate = state.filters.date || "";
  $$('[data-overview-range]').forEach((button) => {
    const selected = !selectedDate && button.dataset.overviewRange === state.overviewRange;
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-pressed", String(selected));
  });
  $$('[data-detail-range]').forEach((button) => {
    const selected = !selectedDate && button.dataset.detailRange === state.detailRange;
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-pressed", String(selected));
  });
  const picker = $("#detailDatePicker");
  if (!picker) return;
  picker.value = selectedDate;
  picker.max = todayKey();
  $(".details-date-picker").classList.toggle("selected", Boolean(selectedDate));
  $("#detailDateLabel").textContent = selectedDate
    ? i18n.formatDate(dateFromKey(selectedDate), { year: "numeric", month: "short", day: "numeric" })
    : t("details.pickDate");
}

function monthBounds(cursor = state.monthCursor) {
  const start = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
  const end = new Date(cursor.getFullYear(), cursor.getMonth() + 1, 1);
  return { since: dateKey(start), until: dateKey(end) };
}

function currentBounds() {
  if (state.view === "daily") return monthBounds();
  if (state.view === "details") return rangeBounds(state.detailRange);
  return rangeBounds(state.overviewRange);
}

const formatToken = (value = 0) => i18n.formatToken(value);
const fullToken = (value = 0) => i18n.formatNumber(Number(value || 0));
const formatUSD = (value = "0") => {
  const amount = Number(value || 0);
  if (!Number.isFinite(amount)) return "—";
  const digits = amount >= 100 ? 2 : amount >= 1 ? 2 : amount >= .01 ? 3 : 6;
  return `$${amount.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: digits })}`;
};
const formatPercent = (value = 0) => `${(Number(value || 0) * 100).toFixed(1)}%`;
const localTime = (value) => value ? i18n.formatDateTime(value, {timeZone:measurementTimezone}) : "—";
const shortId = (value = "") => value.length > 18 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value || "—";
const shortPath = (value = "") => {
  const parts = String(value).split(/[\\/]/).filter(Boolean);
  return parts.at(-1) || value || t("common.notRecorded");
};
const escapeHTML = (value = "") => String(value).replace(/[&<>"']/g, (char) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
}[char]));
const confidenceLabel = (value) => {
  if (!value) return "—";
  const key = `confidence.${value}`;
  const translated = t(key);
  return translated === key ? value : translated;
};

async function api(path, options = {}) {
  const method = String(options.method || "GET").toUpperCase();
  const cacheable = method === "GET" && !path.startsWith("/api/v1/status") && !path.startsWith("/api/v1/warnings") && !path.startsWith("/api/v1/updates") && !path.startsWith("/api/v1/limits");
  const requestGeneration = cacheGeneration;
  const key = `${requestGeneration}:${method} ${path}`;
  if (cacheable) {
    const cached = responseCache.get(key);
    if (cached && cached.expires > Date.now()) return cached.payload;
    if (inflightRequests.has(key)) return inflightRequests.get(key);
  }
  const request = (async () => {
    const response = await fetch(path, { cache: "no-store", ...options });
    const body = await response.text();
    let payload = null;
    if (body) {
      try { payload = JSON.parse(body); } catch { payload = body; }
    }
    if (!response.ok) {
      const message = payload && typeof payload === "object" ? payload.error : payload;
      const error = new Error(serverText(message) || `${response.status} ${response.statusText}`);
      error.status = response.status;
      error.payload = payload;
      throw error;
    }
    if (cacheable && requestGeneration === cacheGeneration) responseCache.set(key, { payload, expires: Date.now() + DATA_CACHE_TTL });
    return payload;
  })();
  if (cacheable) inflightRequests.set(key, request);
  try {
    return await request;
  } finally {
    if (cacheable) inflightRequests.delete(key);
  }
}

function invalidateDataCache() {
  cacheGeneration += 1;
  responseCache.clear();
}

function filterQuery(extra = {}, filters = state.filters) {
  const query = new URLSearchParams();
  const values = { ...filters, ...extra };
  if (filters.date) {
    if (/^\d{4}-\d{2}-\d{2}$/.test(String(values.since || ""))) delete values.since;
    if (/^\d{4}-\d{2}-\d{2}$/.test(String(values.until || ""))) delete values.until;
  }
  for (const [key, value] of Object.entries(values)) {
    if (key === "label") continue;
    if (value !== undefined && value !== null && value !== "" && value !== "all") query.set(key, value);
  }
  return query.toString();
}

function apiURL(path, extra = {}, filters = state.filters) {
  const query = filterQuery({ ...extra, provider: state.provider, ...(["/api/v1/cost-estimate", "/api/v1/sessions", "/api/v1/session-estimates", "/api/v1/session-tree"].includes(path) ? { cost_basis: "codex_fast_weighted" } : {}) }, filters);
  return query ? `${path}?${query}` : path;
}


function modesFor(row = {}) {
  if (row.modes) return row.modes;
  const usage = row.usage || emptyUsage();
  return { regular: usage, fast: emptyUsage(), unknown: usage };
}
function modeText(row = {}) {
  const m = modesFor(row);
  return `${t("mode.totalTokens")} ${formatToken(usageTotal(row.usage))} / Fast ${formatToken(usageTotal(m.fast))}`;
}
function modeDetail(row = {}, compact = false) {
  const m = modesFor(row);
  const fast = `<span class="mode-fast">Fast <b>${formatToken(usageTotal(m.fast))}</b></span>`;
  return compact ? fast : `<span>${t("mode.regular")} ${formatToken(usageTotal(m.regular))}</span> / ${fast}`;
}
function costRulesDate(report = {}) {
  const date = (value) => /^\d{4}-\d{2}-\d{2}$/.test(value || "")
    ? i18n.formatDate(dateFromKey(value), {year: "numeric", month: "long", day: "numeric"}) : "—";
  // The catalog dates describe Codex rates; other sources use their own cards.
  const codexRules = t("mode.ruleDates", {apiDate: date(report.catalog_as_of), fastDate: date(report.fast_rules_as_of)});
  if (state.provider === "codex") return codexRules;
  if (state.provider === "all") return `${codexRules} ${t("provider.note")}`;
  return t("provider.note");
}
function costModes(estimate = {}) {
  return `${t("mode.regular")} ${formatUSD(estimate.regular_mode_usd ?? estimate.usd ?? 0)} / Fast ${formatUSD(estimate.fast_mode_usd || 0)}`;
}
document.addEventListener("click", (event) => {
  const button = event.target.closest?.("[data-mode-filter]");
  if (!button) return;
  state.filters.mode = button.dataset.modeFilter;
  resetDataSelections(); syncFilterForm(); renderFilterChips(); loadCurrentView();
});

function estimateTokens(estimate = {}) {
  return Number(estimate.priced_tokens || 0) + Number(estimate.unpriced_tokens || 0);
}

function estimateLabel(estimate = {}) {
  const total = estimateTokens(estimate);
  if (!total) return t("dynamic.estimateNone");
  const base = t("dynamic.priced", { coverage: formatPercent(estimate.coverage_ratio) });
  return estimate.unpriced_tokens ? `${base} · ${t("dynamic.unpricedSuffix", { tokens: formatToken(estimate.unpriced_tokens) })}` : base;
}

function estimateDisplay(estimate = {}) {
  return estimateTokens(estimate) ? formatUSD(estimate.usd) : "—";
}

// Prefer the translated label for a known reason kind over the service text.
function reasonText(kind, detail) {
  const key = `reason.${kind}`;
  const translated = t(key);
  return translated === key ? serverText(detail) || kind : translated;
}

function reasonSummary(estimate = {}) {
  const reasons = estimate.reasons || [];
  if (!reasons.length) return "";
  return [...new Set(reasons.map((reason) => {
    const key = `reason.${reason.kind}`;
    const translated = t(key);
    return translated === key ? reason.kind : translated;
  }))].join(i18n.getLocale() === "en" ? ", " : "、");
}

function toast(message, isError = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("error", isError);
  node.classList.remove("hidden");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.classList.add("hidden"), 4200);
}

function renderQualityBanner() {
  const banner = $("#coverageBanner");
  const notes = [...new Set([...state.statusQualityNotes, ...state.dataQualityNotes].filter(Boolean))];
  banner.classList.toggle("hidden", notes.length === 0);
  $("#coverageText").textContent = notes.join("；");
}

async function loadStatus() {
  const payload = await api("/api/v1/status");
  state.status = payload;
  const status = payload.status || {};
  if (status.accounting_timezone) measurementTimezone = status.accounting_timezone;
  syncCustomRangeControls();
  $("#measurementTimezone").textContent = t("time.measurement", {zone:measurementTimezone});
  const revision = status.data_revision == null ? null : String(status.data_revision);
  const changed = (state.dataRevision !== null && revision !== null && revision !== state.dataRevision) || (state.pricingRevision != null && payload.pricing_revision != null && state.pricingRevision !== payload.pricing_revision);
  state.pricingRevision = payload.pricing_revision;
  if (changed) {
    invalidateDataCache();
    state.filterOptions = null;
  }
  state.dataRevision = revision;
  const machine = status.machine || {};
  const machineName = machine.label || machine.hostname || t("common.localMachine");
  $("#machineLabel").textContent = machineName;
  $("#machinePill").title = t("dynamic.machineAria", { machine: machineName });
  $("#machinePill").setAttribute("aria-label", t("dynamic.machineAria", { machine: machineName }));
  $("#machineId").textContent = shortId(machine.id || "");
  $("#machineId").title = machine.id || "";
  $("#lastScan").textContent = localTime(status.last_scan);
  $("#sourceStatus").textContent = t("dynamic.jsonlOnly");
  $("#sourceDot").className = "status-dot live";
  $("#versionLabel").textContent = `v${payload.version || "—"}`;
  $("#scanButton").disabled = Boolean(payload.scanning);
  $(".scan-icon").classList.toggle("spin", Boolean(payload.scanning));

  const notes = [];
  if (status.accounting_upgrade_note) notes.push(t("accounting.upgradeNote"));
  if (status.warning_count) notes.push(t("dynamic.warningCount", { count: i18n.formatNumber(status.warning_count) }));
  for (const home of status.codex_homes || []) {
    if (home.warning) notes.push(serverText(home.warning));
  }
  state.statusQualityNotes = notes;
  renderQualityBanner();
  return changed;
}

function hydrateSelect(selector, items, placeholder) {
  const select = $(selector);
  const selected = select.value || state.filters[Object.entries(FILTER_FIELDS).find(([, item]) => item.selector === selector)?.[0]] || "";
  const values = [...new Set((items || []).map((item) => typeof item === "string" ? item : item.key).filter((key) => key && key !== "未知" && key !== "Unknown"))];
  select.innerHTML = `<option value="">${escapeHTML(placeholder)}</option>${values.map((value) => `<option value="${escapeHTML(value)}">${escapeHTML(value)}</option>`).join("")}`;
  if (values.includes(selected)) select.value = selected;
}

function renderFilterOptions() {
  if (!state.filterOptions) return;
  hydrateSelect("#filterModel", state.filterOptions.models, t("filter.allModels"));
  hydrateSelect("#filterSource", state.filterOptions.sources, t("filter.allSources"));
  hydrateSelect("#filterProject", state.filterOptions.projects, t("filter.allProjects"));
}

async function loadFilterOptions({ force = false } = {}) {
  if (state.filterOptions && !force) {
    renderFilterOptions();
    return;
  }
  try {
    state.filterOptions = await api(`/api/v1/dimensions?provider=${encodeURIComponent(state.provider)}`);
    renderFilterOptions();
  } catch (error) {
    toast(t("dynamic.filterLoadError", { error: error.message }), true);
  }
}

function renderFilterChips() {
  const entries = Object.entries(state.filters).filter(([, value]) => value);
  $("#filterContext").classList.toggle("hidden", entries.length === 0);
  $("#filterCount").classList.toggle("hidden", entries.length === 0);
  $("#filterCount").textContent = entries.length;
  $("#filterChips").innerHTML = entries.map(([key, value]) => {
    const display = key === "date" ? i18n.formatDate(dateFromKey(value), { year: "numeric", month: "short", day: "numeric" })
      : key === "project" ? shortPath(value)
      : key === "mode" ? t(value === "regular" ? "mode.regularAssumed" : `mode.${value}`)
      : key === "confidence" ? confidenceLabel(value)
        : key === "session_id" ? shortId(value) : value;
    const label = fieldLabel(key);
    return `<span class="filter-chip" title="${escapeHTML(value)}"><span>${escapeHTML(label)} · ${escapeHTML(display)}</span><button class="pressable" type="button" data-remove-filter="${escapeHTML(key)}" aria-label="${escapeHTML(t("dynamic.removeFilter", { label }))}">×</button></span>`;
  }).join("");
  $$('[data-remove-filter]').forEach((button) => button.addEventListener("click", () => {
    delete state.filters[button.dataset.removeFilter];
    resetDataSelections();
    renderFilterChips();
    syncFilterForm();
    loadCurrentView();
  }));
  syncRangeControls();
}

function syncFilterForm() {
  for (const [key, meta] of Object.entries(FILTER_FIELDS)) $(meta.selector).value = state.filters[key] || "";
  $("#filterDate").max = todayKey();
}

function resetDataSelections() {
  state.hourlyPointDate = "";
  state.pulseDate = "";
  const filteredDate = state.filters.date || "";
  const parsed = dateFromKey(filteredDate);
  if (filteredDate && !Number.isNaN(parsed.getTime()) && dateKey(parsed) === filteredDate) {
    state.hourlyDate = filteredDate;
    state.monthCursor = new Date(parsed.getFullYear(), parsed.getMonth(), 1);
    state.selectedDate = filteredDate;
    state.dailyInitialSelection = false;
    return;
  }
  state.selectedDate = "";
  state.dailyInitialSelection = true;
}

function applyFiltersFromForm() {
  if (!$("#filterForm").reportValidity()) return;
  const filters = {};
  for (const [key, meta] of Object.entries(FILTER_FIELDS)) {
    const value = $(meta.selector).value;
    if (value) filters[key] = value;
  }
  state.filters = filters;
  resetDataSelections();
  renderFilterChips();
  closeDialog($("#filterSheet"));
  loadCurrentView();
}

function clearAllFilters() {
  state.filters = {};
  resetDataSelections();
  syncFilterForm();
  renderFilterChips();
  loadCurrentView();
}

function setLoading(element, label = null) {
  if (label !== null || !element.textContent.trim()) element.textContent = label ?? "—";
  element.classList.add("loading");
}

async function loadOverview({ preserve = false } = {}) {
  const serial = ++state.requestSerial.overview;
  const bounds = rangeBounds(state.overviewRange);
  const custom = state.overviewRange === "custom" && !state.filters.date;
  state.customRangeLoading = custom;
  state.customRangeError = "";
  syncCustomRangeControls();
  $("#overviewSubtitle").textContent = state.filters.date
    ? i18n.formatDate(dateFromKey(state.filters.date), { year: "numeric", month: "long", day: "numeric" })
    : bounds.label;
  if (!preserve) {
    setLoading($("#overviewTotal"), custom ? "—" : null);
    setLoading($("#overviewCost"), custom ? "—" : null);
    if (custom) {
      $("#overviewExactTotal").textContent = "";
      $("#overviewTokenModes").textContent = "";
      $("#overviewTokenBreakdown").textContent = "";
      $("#overviewCostModes").textContent = "";
      $("#overviewCostNote").textContent = "";
      $("#overviewCoverage").textContent = t("overview.estimating");
      $("#overviewCoverageBar").style.width = "0%";
      renderPulse([]);
      renderOverviewModels([]);
    }
  }
  loadHourlyUsage({ preserve: preserve || state.hourlyLoaded });
  $("#view-overview").setAttribute("aria-busy", "true");
  let firstError = null;
  await Promise.all([
    api(apiURL("/api/v1/summary", bounds)).then((summary) => {
      if (serial !== state.requestSerial.overview) return;
      state.overview = { ...(state.overview || {}), summary };
      renderOverviewSummary(summary);
    }).catch((error) => { firstError ||= error; }),
    api(apiURL("/api/v1/cost-estimate", { ...bounds, bucket: "day", ...(custom ? { fill_days: "0" } : {}) })).then((cost) => {
      if (serial !== state.requestSerial.overview) return;
      state.overview = { ...(state.overview || {}), cost };
      renderOverviewCost(cost);
    }).catch((error) => { firstError ||= error; })
  ]);
  if (serial !== state.requestSerial.overview) return;
  $("#view-overview").removeAttribute("aria-busy");
  state.customRangeLoading = false;
  if (firstError) {
    if (custom) {
      state.customRangeError = t("dynamic.overviewError", { error: firstError.message });
      $("#overviewTotal").classList.remove("loading");
      $("#overviewCost").classList.remove("loading");
    } else toast(t("dynamic.overviewError", { error: firstError.message }), true);
  }
  syncCustomRangeControls();
}

async function loadHourlyUsage({ preserve = false } = {}) {
  const serial = ++state.requestSerial.hourly;
  clearTimeout(hourlyContextTimer);
  state.requestSerial.hourlyContext += 1;
  let windows = hourlyWindow();
  state.hourlyDate = windows.date;
  syncHourlyNavigator(windows);
  const chart = $("#hourlyChart");
  const summary = $(".hourly-summary");
  const context = $("#hourlyContext");
  if (!preserve) {
    setLoading($("#hourlyTotal"));
    $("#hourlyLedger").innerHTML = `<span class="hourly-placeholder">${escapeHTML(t("common.loading"))}</span>`;
    setLoading($("#hourlyCost"));
    $("#hourlyCostNote").textContent = t("hourly.costNote");
    $("#hourlyModels").innerHTML = `<span class="hourly-placeholder">${escapeHTML(t("common.loading"))}</span>`;
    $("#hourlyCostCoverage").textContent = "—";
    $("#hourlyCostCoverageNote").textContent = "—";
    $("#hourlyLine").innerHTML = "";
    $("#hourlyAxis").innerHTML = "";
    $("#hourlyPoints").innerHTML = `<div class="empty-state">${escapeHTML(t("common.loading"))}</div>`;
  } else {
    chart.classList.add("is-updating");
    summary.classList.add("is-updating");
    context.classList.add("is-updating");
  }
  summary.setAttribute("aria-busy", "true");
  $("#trendHourlyPane").setAttribute("aria-busy", "true");
  try {
    const series = await api(apiURL("/api/v1/timeseries", {
      date: windows.date, bucket: "hour", complete_hours: 1
    }));
    if (serial !== state.requestSerial.hourly) return;
    if (series.window) { windows = { date:series.window.date,chartStart:new Date(series.window.start),chartEnd:new Date(series.window.end),completeHours:series.window.complete_hours }; }
    state.hourlyWindow = windows;
    syncHourlyNavigator(windows);
    state.hourlyPoints = fillCompleteHours(series.points || [], windows);
    state.hourlyLoaded = true;
    renderHourlyUsage(state.hourlyPoints, windows);
    animateHourlyChart(state.hourlyNavigationDirection);
    state.hourlyNavigationDirection = 0;
    prefetchHourlyNeighbors(windows);
  } catch (error) {
    if (serial === state.requestSerial.hourly) {
      state.hourlyPoints = [];
      renderHourlyUsage([], windows);
      state.hourlyNavigationDirection = 0;
      toast(t("dynamic.hourlyError", { error: error.message }), true);
    }
  } finally {
    if (serial === state.requestSerial.hourly) {
      chart.classList.remove("is-updating");
      summary.classList.remove("is-updating");
      summary.removeAttribute("aria-busy");
      $("#trendHourlyPane").removeAttribute("aria-busy");
    }
  }
}

function syncHourlyNavigator(windows) {
  const picker = $("#hourlyDatePicker");
  picker.value = windows.date;
  picker.max = todayKey();
  syncDatePickerDisplay("hourlyDate", windows.date);
  $("#nextHourDay").disabled = windows.date >= todayKey();
  $("#currentHourDay").disabled = windows.date === todayKey();
  const displayDate = i18n.formatDate(dateFromKey(windows.date), { year: "numeric", month: "long", day: "numeric" });
  $("#hourlyCaption").textContent = t("hourly.completeHours", { date: displayDate, count: windows.completeHours });
  $("#hourlyPoints").setAttribute("aria-label", t("hourly.rulerAria", { date: displayDate }));
}

function prefetchHourlyNeighbors(windows) {
  const dates = [dateKey(addDays(windows.date, -1)), dateKey(addDays(windows.date, 1))]
    .filter((value) => value <= todayKey());
  for (const date of dates) {
    api(apiURL("/api/v1/timeseries", {
      date, complete_hours: 1, bucket: "hour"
    })).catch(() => {});
  }
}

function animateHourlyChart(direction) {
  const chart = $("#hourlyChart");
  chart.classList.remove("enter-previous", "enter-next", "is-updating");
  if (!direction || reducedMotion()) return;
  chart.classList.add(direction < 0 ? "enter-previous" : "enter-next");
  requestAnimationFrame(() => requestAnimationFrame(() => chart.classList.remove("enter-previous", "enter-next")));
}

function navigateHourlyDate(value, direction = 0) {
  const next = normalizeHourlyDate(value);
  if (next === state.hourlyDate && state.hourlyLoaded) {
    syncHourlyNavigator(hourlyWindow(next));
    return;
  }
  state.hourlyNavigationDirection = direction || (next < state.hourlyDate ? -1 : 1);
  state.hourlyDate = next;
  state.hourlyPointDate = "";
  syncHourlyNavigator(hourlyWindow(next));
  loadHourlyUsage({ preserve: state.hourlyLoaded });
}

function renderHourlySelection(point) {
  const totalNode = $("#hourlyTotal");
  if (!point) {
    $("#hourlyModes").innerHTML = "";
    $("#hourlyWindowLabel").textContent = "—";
    totalNode.textContent = "—";
    totalNode.removeAttribute("title");
    totalNode.classList.remove("loading");
    $("#hourlyLedger").innerHTML = `<span class="hourly-placeholder">${escapeHTML(t("dynamic.hourlyEmpty"))}</span>`;
    return;
  }
  const usage = point.usage || emptyUsage();
  const total = usageTotal(usage);
  $("#hourlyModes").innerHTML = modeDetail(point);
  $("#hourlyWindowLabel").textContent = formatHourWindow(point.start, new Date(point.start.getTime() + 60 * 60_000));
  totalNode.textContent = formatToken(total);
  totalNode.title = `${fullToken(total)} Total Token`;
  totalNode.classList.remove("loading");
  $("#hourlyLedger").innerHTML = [
    ["hourlyInput", "Input", usage.input], ["hourlyCached", "Cached", usage.cached_input],
    ["hourlyCacheWrite", "Cache Write", usage.cache_write_input], ["hourlyOutput", "Output", usage.output],
    ["hourlyReasoning", "Reasoning", usage.reasoning_output]
  ].map(([id, name, value]) => `<span><small>${name}</small><strong id="${id}" title="${fullToken(value)}">${formatToken(value)}</strong></span>`).join("");
}

function finishHourlyContextUpdate() {
  const context = $("#hourlyContext");
  context.classList.remove("is-updating");
  context.removeAttribute("aria-busy");
}

function renderHourlyContext(report, point) {
  const estimate = report?.summary || {};
  const pointUsage = point?.usage || emptyUsage();
  const hasUsage = usageTotal(pointUsage) > 0 || estimateTokens(estimate) > 0;
  const pricedTokens = Number(estimate.priced_tokens || 0);
  const costNode = $("#hourlyCost");
  costNode.textContent = !hasUsage ? formatUSD(0)
    : estimateTokens(estimate) && !pricedTokens ? t("dynamic.unpriced")
      : estimateDisplay(estimate);
  costNode.title = hasUsage ? `${formatUSD(estimate.usd)} · ${estimateLabel(estimate)}` : t("hourly.noUsage");
  costNode.classList.remove("loading");
  const reasons = reasonSummary(estimate);
  $("#hourlyCostNote").textContent = `${costModes(estimate)}`;
  $("#hourlyCostNote").title = reasons || costRulesDate(report);

  const models = (report?.models || []).filter((item) => usageTotal(item.usage) > 0);
  const visibleModels = models.slice(0, 3);
  $("#hourlyModels").innerHTML = visibleModels.length ? visibleModels.map((item) => {
    const tokens = usageTotal(item.usage);
    const modelEstimate = item.estimate || {};
    const modelCost = estimateTokens(modelEstimate) && !Number(modelEstimate.priced_tokens || 0)
      ? t("dynamic.unpriced") : estimateDisplay(modelEstimate);
    return `<span class="hourly-model-chip" title="${escapeHTML(`${serverText(item.key)} · ${fullToken(tokens)} Token · ${modelCost}`)}"><strong>${escapeHTML(serverText(item.key) || t("common.unknownModel"))}</strong><small>${escapeHTML(`${formatToken(tokens)} · ${modelCost}`)}</small></span>`;
  }).join("") + (models.length > visibleModels.length ? `<span class="hourly-model-more">${escapeHTML(t("hourly.moreModels", { count: i18n.formatNumber(models.length - visibleModels.length) }))}</span>` : "")
    : `<span class="hourly-placeholder">${escapeHTML(t("hourly.noModels"))}</span>`;

  $("#hourlyCostCoverage").textContent = hasUsage ? formatPercent(estimate.coverage_ratio) : "—";
  $("#hourlyCostCoverageNote").textContent = !hasUsage ? t("hourly.noUsage")
    : Number(estimate.unpriced_tokens || 0) > 0
      ? t("dynamic.unpricedSuffix", { tokens: formatToken(estimate.unpriced_tokens) })
      : t("hourly.catalog", { date: report?.catalog_as_of || "—" });
  finishHourlyContextUpdate();
}

function renderHourlyContextError(error) {
  $("#hourlyCost").textContent = "—";
  $("#hourlyCost").removeAttribute("title");
  $("#hourlyCost").classList.remove("loading");
  $("#hourlyCostNote").textContent = t("dynamic.hourlyContextError", { error: error.message });
  $("#hourlyCostNote").removeAttribute("title");
  $("#hourlyModels").innerHTML = `<span class="hourly-placeholder">—</span>`;
  $("#hourlyCostCoverage").textContent = "—";
  $("#hourlyCostCoverageNote").textContent = "—";
  finishHourlyContextUpdate();
}

function scheduleHourlyContext(point, { immediate = false } = {}) {
  clearTimeout(hourlyContextTimer);
  const serial = ++state.requestSerial.hourlyContext;
  const context = $("#hourlyContext");
  context.classList.add("is-updating");
  context.setAttribute("aria-busy", "true");
  if (!point || usageTotal(point.usage) === 0) {
    renderHourlyContext({ summary: {}, models: [] }, point);
    return;
  }
  const load = async () => {
    const end = new Date(point.start.getTime() + 60 * 60_000);
    try {
      const report = await api(apiURL("/api/v1/cost-estimate", {
        since: point.start.toISOString(),
        until: end.toISOString()
      }));
      if (serial !== state.requestSerial.hourlyContext || state.hourlyPointDate !== point.date) return;
      renderHourlyContext(report, point);
    } catch (error) {
      if (serial !== state.requestSerial.hourlyContext || state.hourlyPointDate !== point.date) return;
      renderHourlyContextError(error);
      toast(t("dynamic.hourlyContextError", { error: error.message }), true);
    }
  };
  if (immediate) void load();
  else hourlyContextTimer = setTimeout(() => { void load(); }, 110);
}

function renderHourlyUsage(points, windows) {
  syncHourlyNavigator(windows);
  renderHourlyLine(points);
  $("#trendHourlyPane").title = t("hourly.updated", { time: localTime(new Date().toISOString()) });
}

function renderHourlyLine(points) {
  const line = $("#hourlyLine");
  const pointLayer = $("#hourlyPoints");
  const axis = $("#hourlyAxis");
  cancelAnimationFrame(pointLayer.hourlyScrubFrame || 0);
  const selectedWindow = state.hourlyWindow || hourlyWindow(state.hourlyDate);
  const displayDate = i18n.formatDate(dateFromKey(selectedWindow.date), { year: "numeric", month: "long", day: "numeric" });
  pointLayer.setAttribute("aria-label", t("hourly.rulerAria", { date: displayDate }));
  if (!points.length) {
    line.innerHTML = "";
    axis.innerHTML = "";
    pointLayer.onpointermove = null;
    pointLayer.innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.hourlyEmpty"))}</div>`;
    renderHourlySelection(null);
    scheduleHourlyContext(null, { immediate: true });
    return;
  }
  const plot = { left: 28, right: 972, top: 16, bottom: 142 };
  const max = Math.max(...points.map((point) => usageTotal(point.usage)), 1);
  const coordinates = points.map((point, index) => {
    const x = plot.left + (plot.right - plot.left) * (points.length === 1 ? .5 : index / (points.length - 1));
    const total = usageTotal(point.usage);
    const y = total ? plot.bottom - (plot.bottom - plot.top) * total / max : plot.bottom;
    return { point, index, total, x, y };
  });
  const fastPolyline = coordinates.map(({point,x}) => `${x.toFixed(2)},${(plot.bottom-(plot.bottom-plot.top)*usageTotal(modesFor(point).fast)/max).toFixed(2)}`).join(" ");
  const polyline = coordinates.map(({ x, y }) => `${x.toFixed(2)},${y.toFixed(2)}`).join(" ");
  const area = `M ${coordinates[0].x.toFixed(2)} ${plot.bottom} L ${polyline.replaceAll(",", " ")} L ${coordinates.at(-1).x.toFixed(2)} ${plot.bottom} Z`;
  line.innerHTML = `<path class="hour-line-area" d="${area}"></path><polyline class="hour-line-path" points="${polyline}"></polyline><polyline class="hour-line-fast" points="${fastPolyline}"></polyline><line class="hour-selection-guide" id="hourSelectionGuide" x1="0" x2="0" y1="${plot.top}" y2="${plot.bottom}"></line>`;

  const availableDates = new Set(points.map((point) => point.date));
  if (!state.hourlyPointDate || !availableDates.has(state.hourlyPointDate)) state.hourlyPointDate = points.at(-1).date;
  pointLayer.innerHTML = coordinates.map(({ point, index, total, x, y }) => {
    const windowLabel = formatHourWindow(point.start, new Date(point.start.getTime() + 60 * 60_000));
    const selected = point.date === state.hourlyPointDate;
    return `<button class="hour-point pressable ${selected ? "selected" : ""} ${total ? "" : "zero"}" type="button" data-hour-point="${index}" style="left:${(x / 10).toFixed(3)}%;top:${y.toFixed(2)}px" aria-pressed="${selected}" tabindex="${selected ? "0" : "-1"}" aria-label="${escapeHTML(`${t("hourly.barAria", { time: windowLabel, tokens: fullToken(total) })} / ${modeText(point)}`)}" title="${escapeHTML(`${t("hourly.barAria", { time: windowLabel, tokens: fullToken(total) })} / ${modeText(point)}`)}"></button>`;
  }).join("");
  axis.innerHTML = coordinates.filter(({ index }) => index % 3 === 0 || index === points.length - 1).map(({ point, index, x }) => {
    const clock = formatClock(point.start);
    const edge = index === 0 ? "edge-start" : index === points.length - 1 ? "edge-end" : "";
    return `<time class="${edge}" datetime="${escapeHTML(point.date)}" style="--label-x:${(x / 10).toFixed(3)}%">${escapeHTML(clock)}</time>`;
  }).join("");

  const selectPoint = (index, { immediateContext = false } = {}) => {
    const selectedPoint = points[index];
    if (!selectedPoint) return;
    state.hourlyPointDate = selectedPoint.date;
    $$('[data-hour-point]', pointLayer).forEach((button) => {
      const selected = Number(button.dataset.hourPoint) === index;
      button.classList.toggle("selected", selected);
      button.setAttribute("aria-pressed", String(selected));
      button.tabIndex = selected ? 0 : -1;
    });
    const guide = $("#hourSelectionGuide");
    guide?.setAttribute("x1", coordinates[index].x.toFixed(2));
    guide?.setAttribute("x2", coordinates[index].x.toFixed(2));
    renderHourlySelection(selectedPoint);
    scheduleHourlyContext(selectedPoint, { immediate: immediateContext });
  };
  $$('[data-hour-point]', pointLayer).forEach((button) => {
    const index = Number(button.dataset.hourPoint);
    button.addEventListener("mouseenter", () => selectPoint(index));
    button.addEventListener("focus", () => selectPoint(index, { immediateContext: true }));
    button.addEventListener("click", () => selectPoint(index, { immediateContext: true }));
    button.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const next = event.key === "Home" ? 0 : event.key === "End" ? points.length - 1 : Math.max(0, Math.min(points.length - 1, index + (event.key === "ArrowRight" ? 1 : -1)));
      $(`[data-hour-point="${next}"]`, pointLayer)?.focus();
    });
  });
  let scrubIndex = -1;
  pointLayer.onpointermove = (event) => {
    if (event.pointerType && event.pointerType !== "mouse" && event.pointerType !== "pen") return;
    const rect = pointLayer.getBoundingClientRect();
    if (!rect.width) return;
    const localX = (event.clientX - rect.left) / rect.width * 1000;
    const next = coordinates.reduce((best, coordinate, index) => Math.abs(coordinate.x - localX) < Math.abs(coordinates[best].x - localX) ? index : best, 0);
    if (next === scrubIndex) return;
    scrubIndex = next;
    cancelAnimationFrame(pointLayer.hourlyScrubFrame || 0);
    pointLayer.hourlyScrubFrame = requestAnimationFrame(() => selectPoint(next));
  };
  selectPoint(Math.max(0, points.findIndex((point) => point.date === state.hourlyPointDate)), { immediateContext: true });
  requestAnimationFrame(() => {
    const selected = $('[data-hour-point].selected', pointLayer);
    const scroller = $(".hourly-chart-scroll");
    if (selected && selected.offsetLeft + selected.offsetWidth > scroller.clientWidth + scroller.scrollLeft) {
      scroller.scrollTo({ left: selected.offsetLeft - scroller.clientWidth + selected.offsetWidth + 12, behavior: reducedMotion() ? "auto" : "smooth" });
    }
  });
}

function switchTrendView(next) {
  if (!["hourly", "daily"].includes(next)) return;
  $$('[data-trend-view]').forEach((button) => {
    const selected = button.dataset.trendView === next;
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-selected", String(selected));
    button.tabIndex = selected ? 0 : -1;
  });
  $("#trendHourlyPane").hidden = next !== "hourly";
  $("#trendDailyPane").hidden = next !== "daily";
  if (next === "hourly" && state.hourlyPoints.length) renderHourlyLine(state.hourlyPoints);
  if (next === "daily" && state.pulsePoints.length) renderPulse(state.pulsePoints);
}

function renderOverviewSummary(summary) {
  const total = usageTotal(summary.usage);
  const totalNode = $("#overviewTotal");
  totalNode.textContent = formatToken(total);
  totalNode.title = `${fullToken(total)} Total Token`;
  $("#overviewExactTotal").textContent = t("range.exactTotal", { tokens: fullToken(total) });
  totalNode.classList.remove("loading");
  const usage = summary.usage || emptyUsage();
  $("#overviewTokenModes").innerHTML = modeDetail(summary);
  $("#overviewTokenBreakdown").innerHTML = [
    ["Input", usage.input], ["Cached", usage.cached_input], ["Cache Write", usage.cache_write_input],
    ["Output", usage.output], ["Reasoning", usage.reasoning_output]
  ].map(([label, value]) => `<span>${label}<b title="${fullToken(value)}">${formatToken(value)}</b></span>`).join("");
  state.dataQualityNotes = [];
  renderQualityBanner();
}

function renderOverviewCost(cost) {
  const estimate = cost.summary || {};
  const costNode = $("#overviewCost");
  $("#overviewCostModes").textContent = costModes(estimate);
  costNode.textContent = estimateDisplay(estimate);
  costNode.classList.remove("loading");
  $("#overviewCoverage").textContent = estimateLabel(estimate);
  $("#overviewCoverageBar").style.width = `${Math.max(0, Math.min(100, Number(estimate.coverage_ratio || 0) * 100))}%`;
  const reasons = reasonSummary(estimate);
  $("#overviewCostNote").textContent = costRulesDate(cost);
  $("#overviewCostNote").title = reasons;
  renderPulse(cost.points || []);
  renderOverviewModels(cost.models || []);
}

function renderPulse(allPoints) {
  state.pulsePoints = allPoints;
  let points = allPoints;
  const limited = points.length > 90;
  if (limited) points = points.slice(-90);
  const caption = $("#pulseCaption");
  if (caption) caption.textContent = limited ? t("dynamic.pulseLimited")
    : state.overviewRange === "custom" && !state.filters.date ? t("range.pulseCaption") : t("pulse.caption");
  const belt = $("#pulseBelt");
  if (!points.length) {
    belt.style.minWidth = "100%";
    belt.innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.pulseEmpty"))}</div>`;
    renderPulseInspector(null);
    return;
  }
  const max = Math.max(...points.map((point) => usageTotal(point.usage)), 1);
  const availableDates = new Set(points.map((point) => point.date));
  if (!state.pulseDate || !availableDates.has(state.pulseDate)) {
    state.pulseDate = [...points].reverse().find((point) => usageTotal(point.usage) > 0)?.date || points.at(-1).date;
  }
  belt.style.minWidth = points.length > 18 ? `${points.length * 38}px` : "100%";
  belt.innerHTML = points.map((point) => {
    const total = usageTotal(point.usage);
    const height = total ? Math.max(5, total / max * 118) : 2;
    const selected = point.date === state.pulseDate;
    const date = dateFromKey(point.date);
    const label = t("dynamic.dayAria", { date: i18n.formatDate(date, { month: "long", day: "numeric" }), tokens: fullToken(total), estimate: estimateLabel(point.estimate) });
    return `<button class="pulse-day pressable ${selected ? "selected" : ""} ${total ? "" : "zero"}" type="button" role="option" data-pulse-date="${point.date}" aria-selected="${selected}" tabindex="${selected ? "0" : "-1"}" aria-label="${escapeHTML(`${label} / ${modeText(point)}`)}"><span class="pulse-bar-space"><i class="pulse-bar" style="--pulse-height:${Math.max(2,usageTotal(point.usage)/max*118)}px"></i><i class="pulse-fast-bar" style="--pulse-height:${usageTotal(modesFor(point).fast)/max*118}px"></i></span><time datetime="${point.date}">${pad2(date.getMonth() + 1)}/${pad2(date.getDate())}</time></button>`;
  }).join("");
  const selectPoint = (date) => {
    state.pulseDate = date;
    $$('[data-pulse-date]', belt).forEach((button) => {
      const selected = button.dataset.pulseDate === date;
      button.classList.toggle("selected", selected);
      button.setAttribute("aria-selected", String(selected));
      button.tabIndex = selected ? 0 : -1;
    });
    renderPulseInspector(points.find((point) => point.date === date));
  };
  $$('[data-pulse-date]', belt).forEach((button) => {
    button.addEventListener("click", () => selectPoint(button.dataset.pulseDate));
    button.addEventListener("focus", () => selectPoint(button.dataset.pulseDate));
    button.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const buttons = $$('[data-pulse-date]', belt);
      const current = buttons.indexOf(button);
      const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1 : Math.max(0, Math.min(buttons.length - 1, current + (event.key === "ArrowRight" ? 1 : -1)));
      buttons[next].focus();
    });
  });
  renderPulseInspector(points.find((point) => point.date === state.pulseDate));
  requestAnimationFrame(() => {
    const selected = $('[data-pulse-date].selected', belt);
    const scroller = $("#pulseScroll");
    if (selected && selected.offsetLeft + selected.offsetWidth > scroller.clientWidth) {
      scroller.scrollTo({ left: selected.offsetLeft - scroller.clientWidth + selected.offsetWidth + 8, behavior: reducedMotion() ? "auto" : "smooth" });
    }
  });
}

function renderPulseInspector(point) {
  if (!point) {
    for (const selector of ["#pulseDate", "#pulseTotal", "#pulseIO", "#pulseCost"]) $(selector).textContent = "—";
    return;
  }
  const date = dateFromKey(point.date);
  $("#pulseDate").textContent = i18n.formatDate(date, { month: "long", day: "numeric", weekday: "short" });
  $("#pulseTotal").textContent = formatToken(usageTotal(point.usage));
  $("#pulseTotal").title = modeText(point);
  $("#pulseIO").textContent = formatToken(usageTotal(modesFor(point).fast));
  $("#pulseCost").textContent = `${estimateDisplay(point.estimate)} · ${formatPercent(point.estimate?.coverage_ratio || 0)}`;
}

function renderOverviewModels(models) {
  $("#modelCount").textContent = models.length ? t("common.models", { count: i18n.formatNumber(models.length) }) : t("dynamic.noModelData");
  const container = $("#overviewModels");
  if (!models.length) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.noModelsRange"))}</div>`;
    return;
  }
  const visible = models.slice(0, 6);
  const max = Math.max(...visible.map((item) => usageTotal(item.usage)), 1);
  container.innerHTML = visible.map((item) => {
    const total = usageTotal(item.usage);
    const cost = estimateDisplay(item.estimate);
    const coverage = estimateTokens(item.estimate) ? formatPercent(item.estimate.coverage_ratio) : t("dynamic.unpriced");
    return `<div class="rank-row"><span class="rank-name" title="${escapeHTML(serverText(item.key))}">${escapeHTML(serverText(item.key))}</span><span class="rank-signal" aria-hidden="true"><i style="width:${Math.max(1, total / max * 100)}%"></i></span><span class="rank-value" title="${fullToken(total)} Token">${formatToken(usageTotal(item.usage))}<small class="mode-fast">Fast ${formatToken(usageTotal(modesFor(item).fast))}</small><small>${cost} · ${coverage}</small></span></div>`;
  }).join("");
}

async function loadDaily({ preserve = false } = {}) {
  const serial = ++state.requestSerial.daily;
  const bounds = monthBounds();
  $("#monthLabel").textContent = i18n.formatDate(state.monthCursor, { year: "numeric", month: "long" });
  syncDailyNavigator();
  if (!preserve) $("#calendarGrid").innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.monthLoading"))}</div>`;
  $("#view-daily").setAttribute("aria-busy", "true");
  try {
    const report = await api(apiURL("/api/v1/cost-estimate", { ...bounds, bucket: "day" }));
    if (serial !== state.requestSerial.daily) return;
    const nonzero = (report.points || []).filter((point) => usageTotal(point.usage) > 0);
    if (state.dailyInitialSelection && !nonzero.some((point) => point.date === todayKey()) && !nonzero.length) {
      // Finding the latest active day only needs the lightweight token series;
      // an all-history cost estimate would unnecessarily price every event.
      const history = await api(apiURL("/api/v1/timeseries", { bucket: "day" }));
      if (serial !== state.requestSerial.daily) return;
      const latestPoint = [...(history.points || [])].reverse().find((point) => usageTotal(point.usage) > 0);
      const latest = latestPoint ? { date: latestPoint.date || dateKey(new Date(latestPoint.time)) } : null;
      state.dailyInitialSelection = false;
      if (latest && !latest.date.startsWith(bounds.since.slice(0, 7))) {
        const latestDate = dateFromKey(latest.date);
        state.monthCursor = new Date(latestDate.getFullYear(), latestDate.getMonth(), 1);
        state.selectedDate = latest.date;
        await loadDaily();
        return;
      }
    }
    state.dailyInitialSelection = false;
    state.monthReport = report;
    chooseDailySelection(report.points || []);
    syncDailyNavigator();
    renderCalendar(report);
    if (state.selectedDate) loadDailySelection(state.selectedDate);
  } catch (error) {
    if (serial === state.requestSerial.daily) toast(t("dynamic.dailyError", { error: error.message }), true);
  } finally {
    if (serial === state.requestSerial.daily) $("#view-daily").removeAttribute("aria-busy");
  }
}

function chooseDailySelection(points) {
  const selected = state.selectedDate && dateFromKey(state.selectedDate);
  if (selected && !Number.isNaN(selected.getTime())
    && selected.getFullYear() === state.monthCursor.getFullYear()
    && selected.getMonth() === state.monthCursor.getMonth()) return;
  const today = points.find((point) => point.date === todayKey());
  if (today && usageTotal(today.usage) > 0) {
    state.selectedDate = today.date;
    return;
  }
  state.selectedDate = [...points].reverse().find((point) => usageTotal(point.usage) > 0)?.date || today?.date || points[0]?.date || "";
}

function syncDailyNavigator() {
  const picker = $("#dailyDatePicker");
  const fallback = dateKey(state.monthCursor);
  picker.value = state.selectedDate || fallback;
  picker.max = todayKey();
  syncDatePickerDisplay("dailyDate", picker.value);
  const currentMonth = new Date(new Date().getFullYear(), new Date().getMonth(), 1);
  $("#nextMonth").disabled = state.monthCursor >= currentMonth;
  $("#currentDay").disabled = state.selectedDate === todayKey();
}

function syncDatePickerDisplay(prefix, value) {
  const date = dateFromKey(value);
  if (!value || Number.isNaN(date.getTime()) || dateKey(date) !== value) return;
  $(`#${prefix}Year`).textContent = i18n.formatDate(date, { year: "numeric" });
  $(`#${prefix}Label`).textContent = i18n.formatDate(date, { month: "short", day: "numeric" });
}

function enableNativeDatePicker(picker) {
  const open = () => {
    try { picker.showPicker?.(); } catch {}
  };
  picker.addEventListener("click", open);
  picker.addEventListener("keydown", (event) => {
    if (!["Enter", " "].includes(event.key)) return;
    event.preventDefault();
    open();
  });
}

function navigateDailyDate(value) {
  const parsed = dateFromKey(value);
  if (!value || Number.isNaN(parsed.getTime()) || dateKey(parsed) !== value || value > todayKey()) {
    syncDailyNavigator();
    return;
  }
  const sameMonth = parsed.getFullYear() === state.monthCursor.getFullYear() && parsed.getMonth() === state.monthCursor.getMonth();
  state.dailyInitialSelection = false;
  if (sameMonth && state.monthReport) {
    selectCalendarDate(value);
    syncDailyNavigator();
    return;
  }
  state.monthCursor = new Date(parsed.getFullYear(), parsed.getMonth(), 1);
  state.selectedDate = value;
  loadDaily();
}

function renderCalendar(report) {
  const points = report.points || [];
  const byDate = new Map(points.map((point) => [point.date, point]));
  const max = Math.max(...points.map((point) => usageTotal(point.usage)), 1);
  const first = new Date(state.monthCursor.getFullYear(), state.monthCursor.getMonth(), 1);
  const daysInMonth = new Date(first.getFullYear(), first.getMonth() + 1, 0).getDate();
  const leading = (first.getDay() + 6) % 7;
  const cells = [];
  for (let index = 0; index < leading; index++) cells.push(`<span class="calendar-blank" aria-hidden="true"></span>`);
  for (let day = 1; day <= daysInMonth; day++) {
    const date = dateKey(new Date(first.getFullYear(), first.getMonth(), day));
    const point = byDate.get(date) || { date, usage: emptyUsage(), estimate: {} };
    const total = usageTotal(point.usage);
    const strength = total ? Math.max(5, total / max * 100) : 0;
    const selected = date === state.selectedDate;
    const hasCost = Number(point.estimate?.priced_tokens || 0) > 0;
    const aria = t("dynamic.calendarDayAria", { day: i18n.formatNumber(day), tokens: fullToken(total), estimate: estimateLabel(point.estimate) });
    cells.push(`<button class="calendar-day pressable ${selected ? "selected" : ""} ${date === todayKey() ? "today" : ""} ${total ? "" : "zero"}" type="button" role="gridcell" data-calendar-date="${date}" aria-selected="${selected}" tabindex="${selected ? "0" : "-1"}" aria-label="${escapeHTML(`${aria} / ${modeText(point)}`)}"><span class="day-number">${i18n.formatNumber(day)}</span>${hasCost ? '<i class="cost-marker" aria-hidden="true"></i>' : ""}<span class="day-usage" aria-hidden="true"><i style="--day-strength:${strength}%"></i></span><span class="day-amount">${formatToken(usageTotal(point.usage))}</span><small class="day-fast-amount">Fast ${formatToken(usageTotal(modesFor(point).fast))}</small></button>`);
  }
  while (cells.length % 7) cells.push(`<span class="calendar-blank" aria-hidden="true"></span>`);
  const grid = $("#calendarGrid");
  grid.innerHTML = cells.join("");
  const monthlyTokens = usageTotal(report.summary?.usage) || points.reduce((sum, point) => sum + usageTotal(point.usage), 0);
  $("#monthSummary").textContent = `${modeText({usage: {total: monthlyTokens}, modes: report.modes})} / ${estimateDisplay(report.summary)}`;
  $$('[data-calendar-date]', grid).forEach((button) => {
    button.addEventListener("click", () => selectCalendarDate(button.dataset.calendarDate));
    button.addEventListener("focus", () => selectCalendarDate(button.dataset.calendarDate));
    button.addEventListener("keydown", calendarKeydown);
  });
}

function calendarKeydown(event) {
  const offsets = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7 };
  if (!(event.key in offsets) && event.key !== "Home" && event.key !== "End") return;
  event.preventDefault();
  const buttons = $$('[data-calendar-date]', $("#calendarGrid"));
  let targetDate;
  if (event.key === "Home") targetDate = buttons[0]?.dataset.calendarDate;
  else if (event.key === "End") targetDate = buttons.at(-1)?.dataset.calendarDate;
  else targetDate = dateKey(addDays(event.currentTarget.dataset.calendarDate, offsets[event.key]));
  const target = buttons.find((button) => button.dataset.calendarDate === targetDate);
  target?.focus();
}

function selectCalendarDate(date) {
  if (!date || date === state.selectedDate) {
    syncDailyNavigator();
    return;
  }
  state.selectedDate = date;
  syncDailyNavigator();
  $$('[data-calendar-date]').forEach((button) => {
    const selected = button.dataset.calendarDate === date;
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-selected", String(selected));
    button.tabIndex = selected ? 0 : -1;
  });
  const point = state.monthReport?.points?.find((item) => item.date === date);
  renderDayDetail(point || { date, usage: emptyUsage(), estimate: {} }, null);
  loadDailySelection(date);
}

async function loadDailySelection(date) {
  const serial = ++state.requestSerial.day;
  const point = state.monthReport?.points?.find((item) => item.date === date) || { date, usage: emptyUsage(), estimate: {} };
  renderDayDetail(point, null, true);
  try {
    const report = await api(apiURL("/api/v1/cost-estimate", { since: date, until: dateKey(addDays(date, 1)), bucket: "day" }));
    if (serial !== state.requestSerial.day || date !== state.selectedDate) return;
    renderDayDetail(report.points?.find((item) => item.date === date) || point, report);
  } catch (error) {
    if (serial === state.requestSerial.day) toast(t("dynamic.selectedError", { error: error.message }), true);
  }
}

function renderDayDetail(point, report, loadingModels = false) {
  const date = dateFromKey(point.date);
  const usage = point.usage || emptyUsage();
  const total = usageTotal(usage);
  const estimate = point.estimate || {};
  const title = i18n.formatDate(date, { month: "long", day: "numeric", weekday: "long" });
  $("#selectedDateTitle").textContent = title;
  $("#selectedDateTitle").dataset.loadedDate = report ? point.date : "";
  $("#selectedDayStatus").textContent = total ? t("dynamic.hasUsage") : t("dynamic.zeroUsage");
  $("#dayTotal").textContent = formatToken(total);
  $("#dayModes").innerHTML = modeDetail(point);
  $("#dayTotal").title = `${fullToken(total)} Token`;
  const regular = Math.max(0, Number(usage.input || 0) - Number(usage.cached_input || 0) - Number(usage.cache_write_input || 0));
  const parts = [
    ["regular", regular, "Regular Input"], ["cached", usage.cached_input, "Cached Input"],
    ["write", usage.cache_write_input, "Cache Write"], ["output", usage.output, "Output"]
  ];
  $("#dayComposition").innerHTML = total ? parts.filter(([, value]) => Number(value || 0) > 0).map(([className, value, label]) => `<i class="${className}" style="width:${Number(value) / total * 100}%" title="${label} ${fullToken(value)}"></i>`).join("") : "";
  $("#dayLedger").innerHTML = [
    ["Input", usage.input], ["Cached", usage.cached_input], ["Cache Write", usage.cache_write_input],
    ["Output", usage.output], ["Reasoning ⊂ Output", usage.reasoning_output]
  ].map(([label, value]) => `<div><dt>${label}</dt><dd title="${fullToken(value)}">${formatToken(value)}</dd></div>`).join("");
  $("#dayCost").textContent = estimateDisplay(estimate);
  $("#dayCost").title = costModes(estimate);
  $("#dayCoverage").textContent = `${estimateLabel(estimate)} · ${costModes(estimate)}`;
  if (loadingModels) {
    $("#dayModels").innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.modelsLoading"))}</div>`;
    $("#dayModelCount").textContent = "";
    return;
  }
  const models = report?.models || [];
  $("#dayModelCount").textContent = models.length ? t("common.modelShort", { count: i18n.formatNumber(models.length) }) : "";
  $("#dayModels").innerHTML = models.length ? models.map((item) => `<div class="mini-model-row"><span title="${escapeHTML(serverText(item.key))}">${escapeHTML(serverText(item.key))}</span><strong title="${fullToken(usageTotal(item.usage))}">${formatToken(usageTotal(item.usage))}<small class="mode-fast">Fast ${formatToken(usageTotal(modesFor(item).fast))}</small></strong></div>`).join("") : `<div class="empty-state">${escapeHTML(t("dynamic.noModelsDay"))}</div>`;
}

async function loadBreakdown({ preserve = false } = {}) {
  const serial = ++state.requestSerial.breakdown;
  const bounds = rangeBounds(state.detailRange);
  const dimension = state.detailDimension;
  $("#breakdownTitle").textContent = t("dynamic.breakdownBy", { dimension: dimensionLabel(dimension) });
  if (!preserve) $("#detailBreakdown").innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.breakdownLoading", { dimension: dimensionLabel(dimension) }))}</div>`;
  $("#breakdownPanel").setAttribute("aria-busy", "true");
  try {
    const breakdown = await api(apiURL("/api/v1/breakdown", { ...bounds, dimension, limit: 100 }));
    if (serial !== state.requestSerial.breakdown) return;
    renderBreakdown(breakdown.items || [], dimension);
  } catch (error) {
    if (serial === state.requestSerial.breakdown) toast(t("dynamic.breakdownError", { error: error.message }), true);
  } finally {
    if (serial === state.requestSerial.breakdown) $("#breakdownPanel").removeAttribute("aria-busy");
  }
}

async function loadSessions({ preserve = false } = {}) {
  const serial = ++state.requestSerial.sessions;
  const estimateSerial = ++state.requestSerial.estimates;
  const bounds = rangeBounds(state.detailRange);
  const query = { ...bounds, limit: 100, compact: 1, q: state.sessionSearch };
  const tree = state.sessionView === "tree";
  const filterKey=JSON.stringify({query,filters:state.filters});
  if(state.treeFilterKey!==filterKey) {state.treeFilterKey=filterKey;state.treeOffset=0;}
  const rowsURL = apiURL(tree ? "/api/v1/session-tree" : "/api/v1/sessions", { ...query, ...(tree ? {limit:20,offset:state.treeOffset} : {include_estimate:0}) });
  const estimatesURL = apiURL("/api/v1/session-estimates", query);
  if (!preserve) $("#sessionRows").innerHTML = `<div class="empty-state">${escapeHTML(t("details.sessionsLoading"))}</div>`;
  $("#sessionsPanel").setAttribute("aria-busy", "true");
  try {
    const sessions = await api(rowsURL);
    if (serial !== state.requestSerial.sessions) return;
    const items = sessions.items || [];
    state.treeItems = items;
    renderSessions(items, { estimatesPending: !tree && items.length > 0 });
    $("#treePagination").hidden = !tree;
    $("#treePrevious").disabled = state.treeOffset === 0;
    $("#treeNext").disabled = state.treeOffset + 20 >= Number(sessions.root_count || 0);
    $("#treeCount").textContent = t("tree.roots",{count:sessions.root_count||0});
    if (!tree && items.length) loadSessionEstimates(estimatesURL, serial, estimateSerial, sessions.data_revision);
  } catch (error) {
    if (serial === state.requestSerial.sessions) toast(t("dynamic.sessionsError", { error: error.message }), true);
  } finally {
    if (serial === state.requestSerial.sessions) $("#sessionsPanel").removeAttribute("aria-busy");
  }
}

async function loadSessionEstimates(url, sessionSerial, estimateSerial, revision) {
  try {
    const payload = await api(url);
    if (sessionSerial !== state.requestSerial.sessions || estimateSerial !== state.requestSerial.estimates) return;
    if (revision != null && payload.data_revision != null && revision !== payload.data_revision) { invalidateDataCache(); return loadSessions({preserve:true}); }
    patchSessionEstimates(payload.items || []);
  } catch (error) {
    if (sessionSerial !== state.requestSerial.sessions || estimateSerial !== state.requestSerial.estimates) return;
    markSessionEstimatesUnavailable();
    toast(t("dynamic.sessionEstimateError", { error: error.message }), true);
  }
}

async function loadDetails({ preserve = false, breakdown = true, sessions = true } = {}) {
  const tasks = [];
  if (breakdown) tasks.push(loadBreakdown({ preserve }));
  if (sessions) tasks.push(loadSessions({ preserve }));
  await Promise.all(tasks);
}

function renderBreakdown(items, dimension) {
  const container = $("#detailBreakdown");
  const total = items.reduce((sum, item) => sum + usageTotal(item.usage), 0);
  $("#breakdownMeta").textContent = items.length ? t("dynamic.breakdownMeta", { count: i18n.formatNumber(items.length), tokens: formatToken(total) }) : t("common.noData");
  if (!items.length) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(t("dynamic.breakdownEmpty", { dimension: dimensionLabel(dimension) }))}</div>`;
    return;
  }
  const max = Math.max(...items.map((item) => usageTotal(item.usage)), 1);
  const canFilter = dimension !== "thread";
  container.innerHTML = items.map((item) => {
    const totalTokens = usageTotal(item.usage);
    const active = state.filters[dimension] === item.key;
    const title = active
      ? t("dynamic.drillCancelTitle", { dimension: dimensionLabel(dimension) })
      : t("dynamic.drillTitle", { dimension: dimensionLabel(dimension) });
    const aria = active
      ? t("dynamic.drillCancelAria", { value: serverText(item.key) })
      : t("dynamic.drillAria", { value: serverText(item.key) });
    return `<div class="breakdown-row"><span class="breakdown-name" title="${escapeHTML(serverText(item.key))}">${escapeHTML(serverText(item.key))}</span><span class="breakdown-track" aria-hidden="true"><i style="width:${Math.max(1, totalTokens / max * 100)}%"></i></span><span class="breakdown-value" title="${fullToken(totalTokens)} Token">${formatToken(usageTotal(item.usage))}<small class="mode-fast">Fast ${formatToken(usageTotal(modesFor(item).fast))}</small></span>${canFilter ? `<button class="breakdown-filter pressable ${active ? "active" : ""}" type="button" data-drill-value="${escapeHTML(item.key)}" title="${escapeHTML(title)}" aria-label="${escapeHTML(aria)}" aria-pressed="${active}">${active ? "×" : "＋"}</button>` : "<span></span>"}</div>`;
  }).join("");
  $$('[data-drill-value]', container).forEach((button) => button.addEventListener("click", () => {
    if (state.filters[dimension] === button.dataset.drillValue) delete state.filters[dimension];
    else state.filters[dimension] = button.dataset.drillValue;
    renderFilterChips();
    syncFilterForm();
    resetDataSelections();
    loadDetails();
  }));
}

function sessionEstimatePresentation(estimate = {}) {
  const totalEstimateTokens = estimateTokens(estimate);
  const pricedTokens = Number(estimate.priced_tokens || 0);
  const cost = totalEstimateTokens && !pricedTokens ? t("dynamic.unpriced") : estimateDisplay(estimate);
  const partialCoverage = pricedTokens > 0 && Number(estimate.coverage_ratio || 0) < 1
    ? formatPercent(estimate.coverage_ratio) : "";
  const title = [estimateLabel(estimate), costModes(estimate), reasonSummary(estimate)].filter(Boolean).join(" · ");
  return { cost, partialCoverage, title };
}

function renderSessionEstimate(cell, estimate) {
  const presentation = sessionEstimatePresentation(estimate);
  cell.classList.remove("pending", "unavailable");
  cell.removeAttribute("aria-busy");
  cell.title = presentation.title;
  cell.innerHTML = `<small class="session-mobile-label">API</small><strong>${escapeHTML(presentation.cost)}</strong>${presentation.partialCoverage ? `<small>${escapeHTML(presentation.partialCoverage)}</small>` : ""}`;
}

function patchSessionEstimates(items) {
  const rows = new Map($$('[data-session-id]', $("#sessionRows")).map((row) => [row.dataset.sessionId, row]));
  const received = new Set();
  for (const item of items) {
    const row = rows.get(item.session_id);
    if (!row) continue;
    const cell = $('[data-session-cost]', row);
    if (cell) {
      received.add(item.session_id);
      renderSessionEstimate(cell, item.estimate || {});
    }
  }
  for (const [sessionID, row] of rows) {
    if (received.has(sessionID)) continue;
    const cell = $('[data-session-cost].pending', row);
    if (cell) renderSessionEstimateUnavailable(cell);
  }
}

function renderSessionEstimateUnavailable(cell) {
  cell.classList.remove("pending");
  cell.classList.add("unavailable");
  cell.removeAttribute("aria-busy");
  cell.title = t("details.costUnavailable");
  cell.innerHTML = `<small class="session-mobile-label">API</small><strong>${escapeHTML(t("details.costUnavailable"))}</strong>`;
}

function markSessionEstimatesUnavailable() {
  $$('[data-session-cost].pending', $("#sessionRows")).forEach(renderSessionEstimateUnavailable);
}

function renderSessions(items, { estimatesPending = false } = {}) {
  const container = $("#sessionRows");
  if (!items.length) {
    const key = state.sessionSearch ? "dynamic.sessionsSearchEmpty" : "dynamic.sessionsEmpty";
    container.innerHTML = `<div class="empty-state">${escapeHTML(t(key))}</div>`;
    return;
  }
  const tree = state.sessionView === "tree";
  const ancestors = [];
  if (tree) items = items.filter(item => { ancestors.length = item.depth;const visible = !ancestors.some(id => state.treeCollapsed.has(id));ancestors.push(item.session_id);return visible; });
  container.innerHTML = items.map((item) => {
    const presentation = sessionEstimatePresentation(item.estimate || {});
    const active = state.filters.session_id === item.session_id;
    const costCell = estimatesPending
      ? `<div class="session-metric session-cost pending" data-session-cost aria-busy="true"><small class="session-mobile-label">API</small><strong>${escapeHTML(t("details.costPending"))}</strong></div>`
      : `<div class="session-metric session-cost" data-session-cost title="${escapeHTML(presentation.title)}"><small class="session-mobile-label">API</small><strong>${escapeHTML(presentation.cost)}</strong>${presentation.partialCoverage ? `<small>${escapeHTML(presentation.partialCoverage)}</small>` : ""}</div>`;
    return `<article class="session-row ${tree ? "task-tree-row" : ""}" style="--task-depth:${Math.min(item.depth||0,6)}" data-session-id="${escapeHTML(item.session_id)}">
    <div class="session-cell task-name">${tree && item.children ? `<button type="button" class="tree-toggle pressable" data-tree-toggle="${escapeHTML(item.session_id)}" aria-expanded="${!state.treeCollapsed.has(item.session_id)}" aria-label="${escapeHTML(t("tree.toggle", {title:serverText(item.title) || item.session_id}))}">${state.treeCollapsed.has(item.session_id) ? "＋" : "−"}</button>` : ""}<strong title="${escapeHTML(serverText(item.title) || item.session_id)}">${escapeHTML(serverText(item.title) || t("dynamic.untitledThread"))}</strong><small title="${escapeHTML(item.session_id)}">${escapeHTML(shortId(item.session_id))}</small>${tree ? `<small class="tree-note">${escapeHTML(item.context_only ? t("tree.context") : t("tree.own"))}${item.relationship_status ? ` · ${escapeHTML(t(`tree.${item.relationship_status}`))}` : ""}${item.forked_from_id ? ` · ${escapeHTML(t("tree.fork", {id:shortId(item.forked_from_id)}))}` : ""}</small>` : ""}</div>
    <div class="session-cell"><span title="${escapeHTML(item.project_path || t("common.notRecorded"))}">${escapeHTML(shortPath(item.project_path))}</span><small title="${escapeHTML(item.project_path || "")}">${escapeHTML(item.project_path || t("common.notRecorded"))}</small></div>
    <div class="session-cell"><strong title="${escapeHTML(item.model || t("common.unknownModel"))}">${escapeHTML(item.model || t("common.unknownModel"))}</strong><span>${escapeHTML(item.source || t("common.unknownSource"))}</span></div>
    <div class="session-cell"><span class="agent-badge">${escapeHTML(item.agent_type || "main")}</span><span class="confidence-badge ${escapeHTML(item.confidence)}">${confidenceLabel(item.confidence)}</span></div>
    <div class="session-metric session-token" title="${fullToken(usageTotal(item.usage))} Token"><small class="session-mobile-label">${t("mode.totalTokens")}</small><strong>${formatToken(usageTotal(item.usage))}</strong><div class="mode-row-detail">${modeDetail(item,true)}</div>${tree && item.children ? `<small class="tree-subtotal" title="${fullToken(usageTotal(item.subtree_usage))}">${escapeHTML(t("tree.subtotal"))} ${formatToken(usageTotal(item.subtree_usage))}</small>` : ""}</div>
    ${costCell}
    <div class="session-cell"><span>${escapeHTML(localTime(item.last_usage))}</span></div>
    <button class="session-filter pressable ${active ? "active" : ""}" type="button" data-session-filter="${escapeHTML(item.session_id)}" aria-pressed="${active}">${escapeHTML(t(active ? "details.cancelSessionFilter" : "details.onlySession"))}</button>
  </article>`;
  }).join("");
  $$('[data-tree-toggle]', container).forEach(button => button.addEventListener("click", () => {const id=button.dataset.treeToggle;if(state.treeCollapsed.has(id)) state.treeCollapsed.delete(id);else state.treeCollapsed.add(id);renderSessions(state.treeItems);const toggle=$$('[data-tree-toggle]',container).find(b=>b.dataset.treeToggle===id);toggle?.focus();}));
  $$('[data-session-filter]', container).forEach((button) => button.addEventListener("click", () => {
    if (state.filters.session_id === button.dataset.sessionFilter) delete state.filters.session_id;
    else state.filters.session_id = button.dataset.sessionFilter;
    renderFilterChips();
    syncFilterForm();
    resetDataSelections();
    loadDetails();
  }));
}

function applySessionSearch() {
  clearTimeout(sessionSearchTimer);
  const value = $("#sessionSearch").value.trim();
  $("#sessionSearchClear").classList.toggle("hidden", !value);
  if (value === state.sessionSearch) return;
  state.sessionSearch = value;
  loadSessions();
}

// Limits show the rate-limit windows each tool reports. It follows the shared
// source switch; "all" charts every tool's limits together.
const CLAUDE_LIMIT_ORDER = ["five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps", "seven_day_cowork"];
const LIMIT_STATES = ["idle", "running", "ok", "disabled", "signed_out", "closed", "error"];
const LIMIT_STATE_DOTS = { ok: "live", running: "live", signed_out: "warn", error: "warn" };
const LIMIT_PROVIDER_NAMES = { codex: "Codex", claude: "Claude Code", antigravity: "Antigravity" };
const LIMIT_CODES = {
  command: "claude auth login",
  credentials: "~/.claude/.credentials.json",
  configCredentials: "$CLAUDE_CONFIG_DIR/.credentials.json",
  rateLimits: "rate_limits",
  auth: "auth.json",
  usage: "/usage",
  anthropicHost: "api.anthropic.com",
  platformHost: "platform.claude.com"
};
const LIMIT_NOTE_KEYS = {
  codex: ["limits.note.codex.source", "limits.note.codex.account", "limits.note.codex.history"],
  claude: ["limits.note.claude.source", "limits.note.claude.login", "limits.note.claude.scope"],
  antigravity: ["limits.note.antigravity.source", "limits.note.antigravity.closed", "limits.note.antigravity.pools"]
};

// Go reports a missing time as year 1, which lands before the Unix epoch.
const validTime = (value) => {
  const time = value ? new Date(value).getTime() : NaN;
  return Number.isFinite(time) && time > 0;
};
const limitPercentValue = (value) => Math.min(100, Math.max(0, Number(value) || 0));
const limitPercent = (value) => `${i18n.formatNumber(limitPercentValue(value), { maximumFractionDigits: 1 })}%`;
const joinList = (items) => items.join(i18n.getLocale() === "en" ? ", " : "、");
// Codex reports ChatGPT's internal plan ids; these are the names its own UI
// shows. "team" is ChatGPT Business since its rename, while "business" is an
// Enterprise plan. Claude and Antigravity already send display names.
const CODEX_PLAN_NAMES = {
  free: "Free", free_workspace: "Free", go: "Go", plus: "Plus", prolite: "Pro 100", pro: "Pro 200", promax: "Pro 500",
  team: "Business", self_serve_business_usage_based: "Business", self_serve_business_prolite: "Business Premium",
  business: "Enterprise", ent26: "Enterprise", enterprise: "Enterprise", hc: "Enterprise", enterprise_cbp_usage_based: "Enterprise",
  enterprise_cbp_automation: "Enterprise (Automation)", enterprise_cbp_trial: "Enterprise (Trial)", finserv: "Enterprise",
  edu: "Edu", education: "Edu", edu_plus: "Edu Plus", edu_pro: "Edu Pro", sci: "Academic Researchers", sci_plus: "Academic Researchers Plus",
  k12: "K-12", guest: "Guest"
};
const planName = (value = "", provider = "") => {
  if (!value || value === "unknown") return "";
  if (provider === "codex" && CODEX_PLAN_NAMES[value.toLowerCase()]) return CODEX_PLAN_NAMES[value.toLowerCase()];
  return value.charAt(0).toUpperCase() + value.slice(1);
};
const translatedOr = (key, fallback) => { const text = t(key); return text === key ? fallback : text; };

// Escapes a catalog entry, then renders its {name} slots as inline code.
function codeText(key, codes = LIMIT_CODES) {
  const slots = Object.fromEntries(Object.keys(codes).map((name) => [name, `{${name}}`]));
  let html = escapeHTML(t(key, slots));
  for (const [name, value] of Object.entries(codes)) html = html.replaceAll(`{${name}}`, `<code>${escapeHTML(value)}</code>`);
  return html;
}

function limitTime(value, options = {}) {
  if (!validTime(value)) return "—";
  const date = new Date(value);
  const year = date.getFullYear() === new Date().getFullYear() ? {} : { year: "numeric" };
  return i18n.formatDateTime(date, { timeZone: measurementTimezone, ...year, month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", ...options });
}

function limitPeriod(start, end) {
  if (!validTime(start)) return limitTime(end);
  if (!validTime(end)) return limitTime(start);
  const sameDay = measuredDateKey(new Date(start)) === measuredDateKey(new Date(end));
  const endText = sameDay
    ? i18n.formatDateTime(new Date(end), { timeZone: measurementTimezone, hour: "2-digit", minute: "2-digit" })
    : limitTime(end);
  return `${limitTime(start)} – ${endText}`;
}

function limitDuration(milliseconds) {
  const total = Math.max(1, Math.ceil(milliseconds / 60_000));
  const days = Math.floor(total / 1440);
  const hours = Math.floor((total % 1440) / 60);
  const minutes = total % 60;
  if (days) return t("limits.duration.days", { days: i18n.formatNumber(days), hours: i18n.formatNumber(hours) });
  if (hours) return t("limits.duration.hours", { hours: i18n.formatNumber(hours), minutes: i18n.formatNumber(minutes) });
  return t("limits.duration.minutes", { minutes: i18n.formatNumber(minutes) });
}

function limitWindowName(minutes) {
  const value = Number(minutes) || 0;
  if (value === 300) return t("limits.window.fiveHour");
  if (value === 10080) return t("limits.window.weekly");
  if (value > 0 && value % 1440 === 0) return t("limits.window.days", { count: i18n.formatNumber(value / 1440) });
  if (value > 0 && value % 60 === 0) return t("limits.window.hours", { count: i18n.formatNumber(value / 60) });
  if (value > 0) return t("limits.window.minutes", { count: i18n.formatNumber(value) });
  return t("limits.window.unknown");
}

// Claude names each limit by key; Codex and Antigravity name the window length
// and list the folded limits (Antigravity model names) underneath.
function limitTitle(provider, window) {
  const keys = window.keys || [];
  const labels = window.labels || [];
  if (provider === "claude") {
    const names = keys.map((key, index) => {
      const id = String(key).split(":").pop();
      // Scoped meters (a model's or a surface's own limit) carry their name.
      if (String(key).includes(":")) return t("limits.claude.scoped", { window: limitWindowName(window.window_minutes), name: labels[index] || id });
      return translatedOr(`limits.claude.${id}`, labels[index] || id);
    });
    return { title: joinList([...new Set(names)]) || limitWindowName(window.window_minutes), detail: "" };
  }
  // Antigravity names its model groups in English; known ones are translated.
  const detail = labels.filter((label) => label && label.toLowerCase() !== provider)
    .map((label) => provider === "antigravity" ? translatedOr(`limits.agGroup.${label}`, label) : label);
  return { title: limitWindowName(window.window_minutes), detail: joinList([...new Set(detail)]) };
}

function limitOrder(provider, window) {
  if (provider === "claude") {
    const index = CLAUDE_LIMIT_ORDER.indexOf(String(window.keys?.[0] || "").split(":").pop());
    return index < 0 ? CLAUDE_LIMIT_ORDER.length : index;
  }
  return Number(window.window_minutes) || Number.MAX_SAFE_INTEGER;
}

// Accounts seen before tracking began are still told apart by their plan and
// the reset of their longest window, until a tracked reading claims them.
function limitAccountLabel(payload, id) {
  if (!id) return t("limits.account.unrecorded");
  const account = (payload.accounts || []).find((item) => item.id === id);
  // Unidentified accounts are only known by plan; each row names its own window.
  if (account?.unknown || id.startsWith("history:")) {
    const plan = planName(account?.plan || id.split(":")[1] || "", payload.provider);
    return plan ? t("limits.account.unknownPlan", { plan }) : t("limits.account.unknown");
  }
  return account?.email || id;
}

function limitAccountPlan(payload, id, window) {
  if (id.startsWith("history:")) return "";
  const account = (payload.accounts || []).find((item) => item.id === id);
  return planName(account?.plan || window?.plan || "", payload.provider);
}

// Signed-in accounts first, newest first; then the unknown ones.
function limitAccountIds(payload) {
  const accounts = [...(payload.accounts || [])]
    .sort((a, b) => Number(Boolean(a.unknown)) - Number(Boolean(b.unknown)) || String(b.last_seen || "").localeCompare(String(a.last_seen || "")));
  const seen = [...(payload.current || []), ...(payload.windows || [])].map((window) => window.account || "");
  const ids = [...new Set([...accounts.map((account) => account.id), ...seen.filter((id) => id)])];
  if (seen.includes("")) ids.push("");
  return ids;
}

// Each unidentified account spans one window, so the filter offers one choice
// per plan ("history:<plan>:") while every row still names its own window.
const LIMIT_UNKNOWN_FILTER = "history:";
const limitAccountMatches = (filter, id = "") => filter === null || (filter.startsWith(LIMIT_UNKNOWN_FILTER) && filter.endsWith(":") ? id.startsWith(filter) : id === filter);

function renderLimitsAccounts(payload) {
  const select = $("#limitsAccount");
  const ids = limitAccountIds(payload);
  const known = ids.filter((id) => !id.startsWith(LIMIT_UNKNOWN_FILTER));
  const plans = new Map();
  for (const id of ids.filter((item) => item.startsWith(LIMIT_UNKNOWN_FILTER))) {
    const prefix = `${LIMIT_UNKNOWN_FILTER}${id.split(":")[1] || ""}:`;
    plans.set(prefix, (plans.get(prefix) || 0) + 1);
  }
  const choices = [...known, ...plans.keys()];
  if (state.limitsAccount !== null && !choices.includes(state.limitsAccount)) state.limitsAccount = null;
  select.innerHTML = `<option value="">${escapeHTML(t("limits.allAccounts"))}</option>${choices.map((id) => {
    if (plans.has(id)) {
      const plan = planName(id.split(":")[1] || "", payload.provider);
      const label = plan ? t("limits.account.unknownPlanAll", { plan, count: i18n.formatNumber(plans.get(id)) }) : t("limits.account.unknownNoPlanAll", { count: i18n.formatNumber(plans.get(id)) });
      return `<option value="a:${escapeHTML(id)}">${escapeHTML(label)}</option>`;
    }
    const plan = limitAccountPlan(payload, id);
    const label = plan ? `${limitAccountLabel(payload, id)} · ${plan}` : limitAccountLabel(payload, id);
    return `<option value="a:${escapeHTML(id)}">${escapeHTML(label)}</option>`;
  }).join("")}`;
  select.value = state.limitsAccount === null ? "" : `a:${state.limitsAccount}`;
  select.disabled = choices.length === 0;
}

function renderLimitsStatus(payload) {
  const provider = payload.provider;
  const status = payload.status || {};
  const name = status.state || "idle";
  const label = LIMIT_STATES.includes(name) ? t(`limits.state.${name}`) : name;
  const detailKey = translatedOr(`limits.stateDetail.${provider}.${name}`, "") ? `limits.stateDetail.${provider}.${name}` : `limits.stateDetail.${name}`;
  const detail = t(detailKey) === detailKey ? "" : codeText(detailKey);
  const meta = [];
  if (name !== "disabled" || validTime(status.last_success)) {
    meta.push([t("limits.lastSuccess"), validTime(status.last_success) ? limitTime(status.last_success) : t("limits.never")]);
  }
  if (name === "error" && validTime(status.last_attempt)) meta.push([t("limits.lastAttempt"), limitTime(status.last_attempt)]);
  const error = status.error ? `<p class="limits-error">${escapeHTML(serverText(status.error))}</p>` : "";
  $("#limitsStatus").innerHTML = `<div class="limits-status-row">
      <span class="status-dot ${LIMIT_STATE_DOTS[name] || "neutral"}" aria-hidden="true"></span>
      <div class="limits-status-copy"><strong>${escapeHTML(label)}</strong>${detail ? `<p>${detail}</p>` : ""}${error}</div>
      ${meta.length ? `<dl class="limits-status-meta">${meta.map(([term, value]) => `<div><dt>${escapeHTML(term)}</dt><dd>${escapeHTML(value)}</dd></div>`).join("")}</dl>` : ""}
    </div>${provider === "claude" ? claudeLimitsControl(payload) : ""}`;
}

// Enabling reads and refreshes Claude Code's saved login, so the switch spells
// out exactly what happens before the button and stays reachable afterwards.
function claudeLimitsControl(payload) {
  if (payload.claude_enabled) {
    return `<div class="limits-claude-toggle"><p>${escapeHTML(t("limits.claude.enabledNote"))}</p><button class="action-button quiet pressable" type="button" data-claude-limits="off">${escapeHTML(t("limits.claude.disable"))}</button></div>`;
  }
  const items = ["limits.consent.reads", "limits.consent.refresh", "limits.consent.hosts", "limits.consent.login", "limits.consent.macos", "limits.consent.scope"];
  return `<div class="limits-consent">
      <h3>${escapeHTML(t("limits.consent.title"))}</h3>
      <p>${codeText("limits.consent.intro")}</p>
      <ul>${items.map((key) => `<li>${codeText(key)}</li>`).join("")}</ul>
      <div class="limits-consent-actions"><button class="action-button primary pressable" type="button" data-claude-limits="on">${escapeHTML(t("limits.claude.enable"))}</button><small>${escapeHTML(t("limits.consent.reversible"))}</small></div>
    </div>`;
}

function limitsEmptyText(payload, kind) {
  const name = payload.status?.state;
  if (state.limitsAccount !== null) return t(`limits.empty.${kind}Account`);
  if (kind === "current") {
    if (payload.provider === "claude" && !payload.claude_enabled) return t("limits.empty.claudeDisabled");
    if (payload.provider === "claude" && name === "signed_out") return t("limits.empty.claudeSignedOut");
    if (payload.provider === "antigravity" && name === "closed") return t("limits.empty.antigravityClosed");
    if (name === "idle" || name === "running") return t("limits.empty.idle");
    if (payload.provider === "codex") return t("limits.empty.codexCurrent");
  }
  return t(`limits.empty.${kind}`);
}

// Tokens used so far in a window and their API-equivalent cost; absent when
// the limit's models can't be told apart.
function limitTokensLine(window) {
  if (window.tokens == null) return "";
  const cost = window.estimate ? `<small class="limit-tokens" title="${escapeHTML(estimateLabel(window.estimate))}">${escapeHTML(t("limits.windowCost", { cost: estimateDisplay(window.estimate) }))}</small>` : "";
  return `<small class="limit-tokens" title="${escapeHTML(t("limits.tokensExact", { tokens: fullToken(window.tokens) }))}">${escapeHTML(t("limits.windowTokens", { tokens: formatToken(window.tokens) }))}</small>${cost}`;
}

// In a model row the row names the group, so a card names only its window.
function limitCard(payload, window, { inRow = false } = {}) {
  const named = limitTitle(payload.provider, window);
  const title = inRow ? limitWindowName(window.window_minutes) : named.title;
  const detail = inRow ? "" : named.detail;
  const last = limitPercentValue(window.last_percent);
  const max = limitPercentValue(window.max_percent);
  const level = last >= 90 ? " critical" : last >= 70 ? " high" : "";
  const resetsAt = validTime(window.resets_at) ? new Date(window.resets_at) : null;
  const remaining = resetsAt ? resetsAt.getTime() - Date.now() : NaN;
  const countdown = !resetsAt ? "" : remaining > 0 ? t("limits.countdown", { time: limitDuration(remaining) }) : t("limits.resetDue");
  // A tool may stop reporting a limit while another one binds (Antigravity's
  // weekly quota under its 5-hour one); show when this one was last read.
  const stale = validTime(window.last_seen) && Date.now() - new Date(window.last_seen).getTime() > 15 * 60_000;
  return `<article class="limit-card${level}">
      <div class="limit-card-head">
        <div class="limit-card-title"><strong>${escapeHTML(title)}</strong>${detail ? `<small title="${escapeHTML(detail)}">${escapeHTML(detail)}</small>` : ""}</div>
        <div class="limit-card-value"><b>${escapeHTML(limitPercent(last))}</b>${max > last ? `<small>${escapeHTML(t("limits.peak", { percent: limitPercent(max) }))}</small>` : ""}</div>
      </div>
      <div class="limit-meter" role="meter" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${last}" aria-label="${escapeHTML(t("limits.usedAria", { title, percent: limitPercent(last) }))}"><i style="width:${last}%"></i>${max > last ? `<span class="limit-peak" style="left:${max}%" title="${escapeHTML(t("limits.peak", { percent: limitPercent(max) }))}"></span>` : ""}</div>
      <div class="limit-card-foot">
        <time datetime="${escapeHTML(window.resets_at || "")}">${escapeHTML(resetsAt ? t("limits.resetsAt", { time: limitTime(window.resets_at) }) : t("limits.resetUnknown"))}</time>
        ${countdown ? `<strong>${escapeHTML(countdown)}</strong>` : ""}
      </div>
      ${limitTokensLine(window)}
      ${stale ? `<small class="limit-card-stale">${escapeHTML(t("limits.readAt", { time: limitTime(window.last_seen) }))}</small>` : ""}
    </article>`;
}

// The overview's merged card: one model group, its 5-hour and weekly limits read together.
function limitModelCard(payload, row, { compact = false } = {}) {
  const lines = row.slots.filter(Boolean).map((window) => {
    const title = limitWindowName(window.window_minutes);
    const last = limitPercentValue(window.last_percent);
    const max = limitPercentValue(window.max_percent);
    const level = last >= 90 ? " critical" : last >= 70 ? " high" : "";
    const resetsAt = validTime(window.resets_at) ? new Date(window.resets_at) : null;
    const remaining = resetsAt ? resetsAt.getTime() - Date.now() : NaN;
    const countdown = !resetsAt ? "" : remaining > 0 ? t("limits.countdown", { time: limitDuration(remaining) }) : t("limits.resetDue");
    // A tool may stop reporting a limit while another one binds; say when it was read.
    const stale = validTime(window.last_seen) && Date.now() - new Date(window.last_seen).getTime() > 15 * 60_000;
    return `<div class="limit-line${level}">
        <div class="limit-line-head"><span>${escapeHTML(title)}</span><b>${escapeHTML(limitPercent(last))}</b></div>
        <div class="limit-meter" role="meter" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${last}" aria-label="${escapeHTML(t("limits.usedAria", { title: `${row.name || t("limits.window.allModels")} · ${title}`, percent: limitPercent(last) }))}"><i style="width:${last}%"></i>${max > last ? `<span class="limit-peak" style="left:${max}%" title="${escapeHTML(t("limits.peak", { percent: limitPercent(max) }))}"></span>` : ""}</div>
        <div class="limit-line-foot">${compact ? "" : `<time datetime="${escapeHTML(window.resets_at || "")}">${escapeHTML(resetsAt ? t("limits.resetsAt", { time: limitTime(window.resets_at) }) : t("limits.resetUnknown"))}</time>`}${countdown ? `<strong>${escapeHTML(countdown)}</strong>` : ""}</div>
        ${limitTokensLine(window)}
        ${stale ? `<small class="limit-card-stale">${escapeHTML(t("limits.readAt", { time: limitTime(window.last_seen) }))}</small>` : ""}
      </div>`;
  }).join("");
  return `<article class="limit-card limit-model-card${compact ? " compact" : ""}">
      <h4>${escapeHTML(row.name || t("limits.window.allModels"))}</h4>
      ${lines}
    </article>`;
}

// Current is each limit's latest reading, untouched ones at 0% included; one past
// its reset reads 0% until the next reading.
function renderLimitsCurrent(payload, active) {
  const container = $("#limitsCurrent");
  $("#limitsCurrentMeta").textContent = active.length ? t("limits.currentMeta", { count: i18n.formatNumber(active.length) }) : "";
  // A removed account picked in the filter says so and can be returned.
  const picked = state.limitsAccount === null ? null : (payload.accounts || []).find((account) => account.id === state.limitsAccount);
  if (picked?.hidden) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(t("limits.hiddenNote"))}
        <div><button class="action-button quiet pressable" type="button" data-limits-unhide="${escapeHTML(picked.id)}">${escapeHTML(t("limits.unhide"))}</button></div>
      </div>`;
    bindLimitHide(container, payload);
    return;
  }
  if (!active.length) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(limitsEmptyText(payload, "current"))}</div>`;
    return;
  }
  const groups = new Map(limitAccountIds(payload).map((id) => [id, []]));
  for (const window of active) {
    const id = window.account || "";
    if (!groups.has(id)) groups.set(id, []);
    groups.get(id).push(window);
  }
  container.innerHTML = [...groups].filter(([, items]) => items.length).map(([id, items]) => {
    const plan = limitAccountPlan(payload, id, items[0]);
    const label = limitAccountLabel(payload, id);
    const rows = limitModelRows(payload.provider, items);
    const remove = id && !id.startsWith(LIMIT_UNKNOWN_FILTER)
      ? `<button class="text-button pressable limits-hide" type="button" data-limits-hide="${escapeHTML(id)}" title="${escapeHTML(t("limits.hideTitle"))}">${escapeHTML(t("limits.hide"))}</button>`
      : "";
    return `<section class="limits-account-group">
        <h3 class="limits-account-head"><span title="${escapeHTML(label)}">${escapeHTML(label)}</span>${plan ? `<b class="limits-plan">${escapeHTML(plan)}</b>` : ""}${remove}</h3>
        ${rows.map((row) => `<div class="limits-model-row">
            ${rows.length > 1 || row.name ? `<h4>${escapeHTML(row.name || t("limits.window.allModels"))}</h4>` : ""}
            <div class="limits-cards limits-row-cards">${row.slots.map((window) => window
              ? limitCard(payload, window, { inRow: true })
              : `<div class="limit-card limit-card-empty">${escapeHTML(t("limits.window.none"))}</div>`).join("")}</div>
          </div>`).join("")}
      </section>`;
  }).join("");
  bindLimitHide(container, payload);
}

// Removing an account takes it out of current here and in the overview until
// a newer reading of it arrives; returning it undoes that.
function bindLimitHide(container, payload) {
  const update = async (button, account, hidden) => {
    button.disabled = true;
    const label = limitAccountLabel(payload, account);
    try {
      await api("/api/v1/limits/hidden", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ provider: payload.provider, account, hidden })
      });
      toast(t(hidden ? "limits.hiddenToast" : "limits.unhiddenToast", { account: label }));
      await loadLimits({ preserve: true });
    } catch (error) {
      button.disabled = false;
      toast(t("limits.hideError", { error: error.message }), true);
    }
  };
  $$("[data-limits-hide]", container).forEach((button) => button.addEventListener("click", () => update(button, button.dataset.limitsHide, true)));
  $$("[data-limits-unhide]", container).forEach((button) => button.addEventListener("click", () => update(button, button.dataset.limitsUnhide, false)));
}

// One row per model group (Antigravity's groups, Claude's model-specific
// limits), with the 5-hour limit and the weekly one side by side.
function limitModelRows(provider, items) {
  const rows = new Map();
  for (const window of items) {
    const { detail } = limitTitle(provider, window);
    let name = detail;
    if (provider === "claude") {
      const key = String(window.keys?.[0] || "");
      name = ["five_hour", "seven_day"].includes(key) ? "" : key.includes(":") ? window.labels?.[0] || key : translatedOr(`limits.claudeModel.${key}`, key);
    }
    if (!rows.has(name)) rows.set(name, []);
    rows.get(name).push(window);
  }
  return [...rows].map(([name, windows]) => {
    const fiveHour = windows.find((window) => Number(window.window_minutes) === 300);
    const weekly = windows.find((window) => Number(window.window_minutes) === 10080);
    const rest = windows.filter((window) => window !== fiveHour && window !== weekly);
    // Keep the columns aligned when a group has only one of the two limits.
    const slots = fiveHour || weekly ? [fiveHour || null, weekly || null, ...rest] : rest;
    return { name, slots, order: Math.min(...windows.map((window) => limitOrder(provider, window))) };
  }).sort((a, b) => Number(Boolean(a.name)) - Number(Boolean(b.name)) || a.order - b.order || a.name.localeCompare(b.name));
}

// Past windows, the 5-hour ones and the weekly ones each in their own list.
const LIMIT_HISTORY_KINDS = [
  { key: "fiveHour", match: (minutes) => minutes === 300 },
  { key: "weekly", match: (minutes) => minutes === 10080 },
  { key: "other", match: (minutes) => minutes !== 300 && minutes !== 10080 }
];

// The 5-hour and weekly limits are shown one at a time, switched by buttons;
// the chart and the history share the choice.
function limitKindSwitch(kinds) {
  if (kinds.length < 2) return "";
  return `<div class="segmented limits-kind" role="tablist" aria-label="${escapeHTML(t("limits.kindAria"))}">${kinds.map((kind) => `<button class="pressable${kind.key === state.limitsKind ? " selected" : ""}" type="button" role="tab" aria-selected="${kind.key === state.limitsKind}" data-limits-kind="${kind.key}">${escapeHTML(t(`limits.chart.group.${kind.key}`))}<small>${escapeHTML(i18n.formatNumber(kind.count))}</small></button>`).join("")}</div>`;
}

// Keeps the shared choice on a kind that has data, preferring the current one.
function pickLimitKind(kinds) {
  if (kinds.length && !kinds.some((kind) => kind.key === state.limitsKind)) state.limitsKind = kinds[0].key;
}

function bindLimitKindSwitch(container, redraw) {
  $$("[data-limits-kind]", container).forEach((button) => button.addEventListener("click", () => {
    state.limitsKind = button.dataset.limitsKind;
    redraw();
  }));
}

function renderLimitsHistory(payload, windows) {
  const container = $("#limitsHistory");
  const past = windows.filter((window) => !window.active);
  $("#limitsHistoryMeta").textContent = past.length ? t("limits.historyMeta", { count: i18n.formatNumber(past.length) }) : "";
  if (!past.length) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(limitsEmptyText(payload, "history"))}</div>`;
    return;
  }
  const mobileLabel = (key) => `<span class="limits-mobile-label">${escapeHTML(t(key))}</span>`;
  const head = `<div class="limits-history-head" aria-hidden="true"><span>${escapeHTML(t("limits.account"))}</span><span>${escapeHTML(t("limits.headerWindow"))}</span><span>${escapeHTML(t("limits.headerPeriod"))}</span><span>${escapeHTML(t("limits.headerMax"))}</span><span>${escapeHTML(t("limits.headerTokens"))}</span><span>${escapeHTML(t("limits.headerCost"))}</span></div>`;
  // Both the 5-hour and the weekly button are always there, so every tool's
  // history looks alike before its first weekly window has ended.
  const kinds = LIMIT_HISTORY_KINDS.map((kind) => ({ ...kind, rows: past.filter((window) => kind.match(Number(window.window_minutes) || 0)) }))
    .filter((kind) => kind.key !== "other" || kind.rows.length).map((kind) => ({ ...kind, count: kind.rows.length }));
  pickLimitKind(kinds);
  container.innerHTML = limitKindSwitch(kinds) + kinds.filter((kind) => kind.key === state.limitsKind).map((kind) => {
    const rows = kind.rows;
    if (!rows.length) return `<div class="empty-state">${escapeHTML(t(`limits.historyEmpty.${kind.key}`))}</div>`;
    const limit = state.limitsHistoryLimit[kind.key] || LIMITS_HISTORY_PAGE;
    const remaining = rows.length - limit;
    const more = remaining > 0
      ? `<div class="limits-more"><button class="action-button quiet pressable" type="button" data-limits-more="${kind.key}">${escapeHTML(t("limits.showMore", { count: i18n.formatNumber(Math.min(remaining, LIMITS_HISTORY_PAGE)), remaining: i18n.formatNumber(remaining) }))}</button></div>`
      : "";
    return `<section class="limits-history-block">
        ${head}
        <div class="limits-history-list">${rows.slice(0, limit).map((window) => {
          const id = window.account || "";
          const label = limitAccountLabel(payload, id);
          const plan = limitAccountPlan(payload, id, window);
          const { title, detail } = limitTitle(payload.provider, window);
          const max = limitPercentValue(window.max_percent);
          const hasTokens = window.tokens != null;
          return `<div class="limits-history-row">
              <div class="limits-cell">${mobileLabel("limits.account")}<strong title="${escapeHTML(label)}">${escapeHTML(label)}</strong>${plan ? `<small>${escapeHTML(plan)}</small>` : ""}</div>
              <div class="limits-cell">${mobileLabel("limits.headerWindow")}<strong>${escapeHTML(detail || title)}</strong>${detail ? `<small>${escapeHTML(title)}</small>` : ""}</div>
              <div class="limits-cell limits-period">${mobileLabel("limits.headerPeriod")}<span>${escapeHTML(limitPeriod(window.starts_at, window.ended_at || window.resets_at))}</span>${window.ended_at ? `<small title="${escapeHTML(t("limits.earlyResetDetail", { time: limitTime(window.resets_at) }))}">${escapeHTML(t("limits.earlyReset"))}</small>` : ""}</div>
              <div class="limits-metric">${mobileLabel("limits.headerMax")}<strong>${escapeHTML(limitPercent(max))}</strong><span class="limit-mini${max >= 90 ? " critical" : max >= 70 ? " high" : ""}" aria-hidden="true"><i style="width:${max}%"></i></span></div>
              <div class="limits-metric">${mobileLabel("limits.headerTokens")}<strong${hasTokens ? ` title="${escapeHTML(t("limits.tokensExact", { tokens: fullToken(window.tokens) }))}"` : ""}>${hasTokens ? escapeHTML(formatToken(window.tokens)) : "—"}</strong></div>
              <div class="limits-metric">${mobileLabel("limits.headerCost")}<strong${window.estimate ? ` title="${escapeHTML(estimateLabel(window.estimate))}"` : ""}>${window.estimate ? escapeHTML(estimateDisplay(window.estimate)) : "—"}</strong></div>
            </div>`;
        }).join("")}${more}</div>
      </section>`;
  }).join("");
  bindLimitKindSwitch(container, renderLimits);
}

function renderLimitsNote(provider) {
  const keys = [...(LIMIT_NOTE_KEYS[provider] || []), "limits.note.tokens"];
  $("#limitsNote").innerHTML = `<ul class="limits-note-list">${keys.map((key) => `<li>${codeText(key)}</li>`).join("")}</ul>`;
}

function renderLimits() {
  const payload = state.limits;
  if (!payload) return;
  renderLimitsAccounts(payload);
  const mine = (window) => limitAccountMatches(state.limitsAccount, window.account || "");
  renderLimitsStatus(payload);
  renderLimitsCurrent(payload, (payload.current || []).filter(mine));
  renderLimitsHistory(payload, (payload.windows || []).filter(mine));
}

function scheduleLimitsRefresh(delay = LIMITS_REFRESH_MS) {
  clearTimeout(limitsRefreshTimer);
  if (state.view !== "limits" || document.visibilityState !== "visible") return;
  limitsRefreshTimer = setTimeout(() => { if (state.view === "limits") loadLimits({ preserve: true }); }, delay);
}

// The overview chart: every limit's recorded readings, one color per tool and
// one dash per window length. A change of reset time starts a new window, so
// each window is its own step line that ends at its reset.
const limitSeriesId = (series) => `${series.provider}|${series.account}|${series.key}`;

function limitSeriesName(series, payloads) {
  const payload = payloads[series.provider] || { provider: series.provider, accounts: [] };
  const { title, detail } = limitTitle(series.provider, { keys: [series.key], labels: [series.label], window_minutes: series.window_minutes });
  const accounts = new Set((payloads.series || []).filter((item) => item.provider === series.provider).map((item) => item.account));
  const account = accounts.size > 1 ? series.email || limitAccountLabel(payload, series.account) : "";
  return [LIMIT_PROVIDER_NAMES[series.provider], detail || title, detail ? title : "", account].filter(Boolean).join(" · ");
}

// Reported reset times drift by a few seconds within one window, as in the store.
const LIMIT_RESET_TOLERANCE_MS = 2 * 60_000;

// Splits a series into its windows. A window's line ends at its reset, or
// where the next window began when an early reset started that one sooner.
function limitSegments(series, until) {
  const segments = [];
  for (const point of series.points || []) {
    const resets = new Date(point.resets_at).getTime();
    let segment = segments.find((item) => Math.abs(item.resets - resets) <= LIMIT_RESET_TOLERANCE_MS);
    if (!segment) segments.push(segment = { resets, points: [] });
    segment.points.push(point);
  }
  for (const segment of segments) {
    segment.points.sort((a, b) => new Date(a.t) - new Date(b.t));
    segment.first = new Date(segment.points[0].t).getTime();
  }
  segments.sort((a, b) => a.first - b.first);
  return segments.map((segment, index) => {
    const next = segments[index + 1];
    let end = Math.min(segment.resets, until);
    if (next && next.first < end) end = Math.max(segment.first, next.first);
    return { points: segment.points, first: segment.first, end };
  });
}

function limitValueAt(series, time, until) {
  const segment = limitSegments(series, until).filter((item) => time >= item.first && time <= item.end).at(-1);
  const point = segment && [...segment.points].reverse().find((item) => new Date(item.t).getTime() <= time);
  return point ? { point, resets: point.resets_at } : null;
}

// Draws one chart per window length: 5-hour limits and weekly limits read
// differently, and mixing them made both unreadable. Unidentified accounts
// are hidden until asked for, so the signed-in accounts stay legible.
const LIMIT_CHART_GROUPS = [
  { key: "fiveHour", match: (minutes) => minutes === 300 },
  { key: "weekly", match: (minutes) => minutes === 10080 },
  { key: "other", match: (minutes) => minutes !== 300 && minutes !== 10080 }
];
const isUnknownAccount = (id = "") => id.startsWith("history:");

function renderLimitsChart(data, payloads) {
  const container = $("#limitsChart");
  const since = new Date(data.since).getTime();
  const until = new Date(data.until).getTime();
  const all = (data.series || []).filter((item) => (item.points || []).length)
    .sort((a, b) => LIMIT_PROVIDERS.indexOf(a.provider) - LIMIT_PROVIDERS.indexOf(b.provider) || String(a.account).localeCompare(String(b.account)) || String(a.key).localeCompare(String(b.key)));
  payloads.series = all;
  const unknown = all.filter((item) => isUnknownAccount(item.account));
  const series = state.limitsShowUnknown ? all : all.filter((item) => !isUnknownAccount(item.account));
  $("#limitsChartMeta").textContent = series.length ? t("limits.chart.meta", { count: i18n.formatNumber(series.length) }) : "";
  const toggle = unknown.length
    ? `<button class="text-button pressable limits-unknown-toggle" type="button" data-limits-unknown aria-pressed="${state.limitsShowUnknown}">${escapeHTML(t(state.limitsShowUnknown ? "limits.chart.hideUnknown" : "limits.chart.showUnknown", { count: i18n.formatNumber(unknown.length) }))}</button>`
    : "";
  const available = LIMIT_CHART_GROUPS.map((group) => ({ ...group, series: series.filter((item) => group.match(Number(item.window_minutes) || 0)) }))
    .filter((group) => group.series.length).map((group) => ({ ...group, count: group.series.length }));
  pickLimitKind(available);
  const groups = available.filter((group) => group.key === state.limitsKind);
  container.innerHTML = limitKindSwitch(available) + (groups.length
    ? groups.map((group) => `<section class="limits-chart-group" data-limits-group="${group.key}">
        <div class="limits-chart-body"></div>
        <div class="limits-legend"></div>
      </section>`).join("")
    : `<div class="empty-state">${escapeHTML(t(all.length ? "limits.chart.onlyUnknown" : "limits.chart.empty"))}</div>`) + toggle;
  $("[data-limits-unknown]", container)?.addEventListener("click", () => {
    state.limitsShowUnknown = !state.limitsShowUnknown;
    renderLimitsChart(data, payloads);
  });
  bindLimitKindSwitch(container, () => renderLimitsChart(data, payloads));
  for (const group of groups) {
    const section = $(`[data-limits-group="${group.key}"]`, container);
    drawLimitChart($(".limits-chart-body", section), $(".limits-legend", section), group.series, since, until, payloads, () => renderLimitsChart(data, payloads));
  }
}

function drawLimitChart(body, legend, series, since, until, payloads, redraw) {
  const plot = { top: 10, bottom: 190 };
  const height = 200;
  const x = (time) => (time - since) / Math.max(1, until - since) * 1000;
  const y = (percent) => plot.bottom - (plot.bottom - plot.top) * limitPercentValue(percent) / 100;
  const hidden = state.limitsHiddenSeries;
  const visible = series.filter((item) => !hidden.has(limitSeriesId(item)));
  // Within one chart the window length is fixed, so a shade of the tool's
  // color and a dash tell its accounts apart.
  const accounts = [...new Set(series.map((item) => `${item.provider}|${item.account}`))];
  const accountIndex = (item) => Math.max(0, accounts.filter((id) => id.startsWith(`${item.provider}|`)).indexOf(`${item.provider}|${item.account}`));
  const dashFor = (item) => ["", "6 4", "2 4", "10 3 2 3"][accountIndex(item) % 4];
  const toneFor = (item) => `${item.provider}${["", " shade-1", " shade-2"][accountIndex(item) % 3]}`;
  const paths = visible.map((item) => limitSegments(item, until).map((segment) => {
    let d = "";
    segment.points.forEach((point, index) => {
      const px = x(Math.max(since, new Date(point.t).getTime())).toFixed(2);
      const py = y(point.percent).toFixed(2);
      d += index ? ` H ${px} V ${py}` : `M ${px} ${py}`;
    });
    d += ` H ${x(segment.end).toFixed(2)}`;
    const dash = dashFor(item);
    return `<path class="limits-line ${toneFor(item)}" d="${d}"${dash ? ` stroke-dasharray="${dash}"` : ""}></path>`;
  }).join("")).join("");
  const grid = [0, 25, 50, 75, 100].map((percent) => `<line class="limits-grid" x1="0" x2="1000" y1="${y(percent)}" y2="${y(percent)}"></line>`).join("");
  const yLabels = [0, 50, 100].map((percent) => `<span style="top:${y(percent)}px">${escapeHTML(limitPercent(percent))}</span>`).join("");
  const span = until - since;
  const ticks = Array.from({ length: 5 }, (_, index) => since + span * index / 4);
  const tickText = (time) => span <= 36 * 3600_000
    ? i18n.formatDateTime(new Date(time), { timeZone: measurementTimezone, hour: "2-digit", minute: "2-digit" })
    : i18n.formatDate(new Date(time), { timeZone: measurementTimezone, month: "short", day: "numeric" });
  body.innerHTML = `<div class="limits-chart-frame">
      <div class="limits-y-axis" aria-hidden="true">${yLabels}</div>
      <div class="limits-plot" tabindex="0" role="img" aria-label="${escapeHTML(t("limits.chart.aria", { count: i18n.formatNumber(visible.length) }))}">
        <svg viewBox="0 0 1000 ${height}" preserveAspectRatio="none" aria-hidden="true">${grid}${paths}<line class="limits-guide" x1="0" x2="0" y1="${plot.top}" y2="${plot.bottom}" visibility="hidden"></line></svg>
        <div class="limits-tooltip" hidden></div>
      </div>
    </div>
    <div class="limits-x-axis" aria-hidden="true">${ticks.map((time, index) => `<time class="${index === 0 ? "edge-start" : index === ticks.length - 1 ? "edge-end" : ""}" style="--label-x:${(index * 25).toFixed(2)}%">${escapeHTML(tickText(time))}</time>`).join("")}</div>`;
  legend.innerHTML = series.map((item) => {
    const id = limitSeriesId(item);
    const name = limitSeriesName(item, payloads);
    const dash = dashFor(item);
    const off = hidden.has(id);
    return `<button class="limits-legend-item pressable${off ? " off" : ""}" type="button" data-limits-series="${escapeHTML(id)}" aria-pressed="${!off}" title="${escapeHTML(t("limits.legend.toggle", { name }))}"><svg viewBox="0 0 24 8" aria-hidden="true"><line class="limits-line ${toneFor(item)}" x1="1" x2="23" y1="4" y2="4"${dash ? ` stroke-dasharray="${dash}"` : ""}></line></svg><span>${escapeHTML(name)}</span></button>`;
  }).join("");
  $$("[data-limits-series]", legend).forEach((button) => button.addEventListener("click", () => {
    const id = button.dataset.limitsSeries;
    if (hidden.has(id)) hidden.delete(id); else hidden.add(id);
    redraw();
  }));
  const plotNode = $(".limits-plot", body);
  const guide = $(".limits-guide", body);
  const tooltip = $(".limits-tooltip", body);
  let cursor = until;
  const show = (time) => {
    cursor = Math.min(until, Math.max(since, time));
    const rows = visible.map((item) => ({ item, found: limitValueAt(item, cursor, until) })).filter((row) => row.found)
      .sort((a, b) => b.found.point.percent - a.found.point.percent);
    const position = x(cursor) / 10;
    guide.setAttribute("x1", x(cursor).toFixed(2));
    guide.setAttribute("x2", x(cursor).toFixed(2));
    guide.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    tooltip.classList.toggle("flip", position > 60);
    tooltip.style.left = `${position}%`;
    tooltip.innerHTML = `<strong>${escapeHTML(limitTime(new Date(cursor).toISOString()))}</strong>${rows.length ? rows.slice(0, 8).map(({ item, found }) => `<div class="limits-tooltip-row"><i class="limits-swatch ${toneFor(item)}" aria-hidden="true"></i><span>${escapeHTML(limitSeriesName(item, payloads))}</span><b>${escapeHTML(limitPercent(found.point.percent))}</b><small>${escapeHTML(t("limits.resetsAt", { time: limitTime(found.resets) }))}</small></div>`).join("") : `<div class="limits-tooltip-empty">${escapeHTML(t("limits.chart.noValue"))}</div>`}`;
  };
  const hide = () => { guide.setAttribute("visibility", "hidden"); tooltip.hidden = true; };
  plotNode.addEventListener("pointermove", (event) => {
    const rect = plotNode.getBoundingClientRect();
    if (rect.width) show(since + (event.clientX - rect.left) / rect.width * (until - since));
  });
  plotNode.addEventListener("pointerleave", hide);
  plotNode.addEventListener("blur", hide);
  plotNode.addEventListener("focus", () => show(cursor));
  plotNode.addEventListener("keydown", (event) => {
    const step = (until - since) / 48;
    const next = { ArrowLeft: cursor - step, ArrowRight: cursor + step, Home: since, End: until }[event.key];
    if (next === undefined) return;
    event.preventDefault();
    show(next);
  });
}

// Under the chart, each tool's current limits for signed-in accounts.
function renderLimitsSummary(payloads) {
  $("#limitsSummary").innerHTML = LIMIT_PROVIDERS.map((provider) => {
    const payload = payloads[provider];
    if (!payload) return "";
    const name = payload.status?.state || "idle";
    const rows = (payload.current || []).filter((window) => !String(window.account).startsWith("history:"))
      .sort((a, b) => String(a.account).localeCompare(String(b.account)) || limitOrder(provider, a) - limitOrder(provider, b));
    let body;
    if (provider === "claude" && !payload.claude_enabled) body = `<p class="limits-summary-note">${escapeHTML(t("limits.empty.claudeDisabled"))}</p>`;
    else if (provider === "antigravity" && name === "closed" && !rows.length) body = `<p class="limits-summary-note">${escapeHTML(t("limits.empty.antigravityClosed"))}</p>`;
    else if (!rows.length) body = `<p class="limits-summary-note">${escapeHTML(limitsEmptyText(payload, "current"))}</p>`;
    else {
      // Per account, one row per model group with its 5-hour and weekly limits side by side.
      const accounts = new Map();
      for (const window of rows) {
        if (!accounts.has(window.account)) accounts.set(window.account, []);
        accounts.get(window.account).push(window);
      }
      body = [...accounts].map(([id, windows]) => {
        const plan = limitAccountPlan(payload, id, windows[0]);
        const label = limitAccountLabel(payload, id);
        return `<div class="limits-summary-account">
            <div class="limits-summary-who"><span title="${escapeHTML(label)}">${escapeHTML(label)}</span>${plan ? `<b class="limits-plan">${escapeHTML(plan)}</b>` : ""}</div>
            <div class="limits-summary-cards">${limitModelRows(provider, windows).map((row) => limitModelCard(payload, row, { compact: true })).join("")}</div>
          </div>`;
      }).join("");
    }
    return `<section class="limits-summary-tool">
        <h3><i class="limits-swatch ${provider}" aria-hidden="true"></i><button class="text-button pressable" type="button" data-limits-open="${provider}">${escapeHTML(LIMIT_PROVIDER_NAMES[provider])}</button><span class="limits-summary-state">${escapeHTML(LIMIT_STATES.includes(name) ? t(`limits.state.${name}`) : name)}</span></h3>
        ${body}
      </section>`;
  }).join("");
  $$("[data-limits-open]", $("#limitsSummary")).forEach((button) => button.addEventListener("click", () => switchProvider(button.dataset.limitsOpen)));
}

// A chosen range uses the dashboard's custom-range form; it starts from the
// range on screen.
function openLimitsRange() {
  if (!state.limitsCustom) {
    const now = new Date();
    const days = Number(state.limitsDays) || 7;
    state.limitsCustom = { since: measuredMinute(new Date(now.getTime() - days * 24 * 3600_000)), until: measuredMinute(now) };
  }
  $("#limitsRangeStart").value = state.limitsCustom.since;
  $("#limitsRangeEnd").value = state.limitsCustom.until;
  $("#limitsRangeHint").textContent = t("range.hint", { zone: measurementTimezone });
  $("#limitsRangeError").hidden = true;
  $("#limitsRangeForm").hidden = false;
  $("#limitsRangeStart").focus();
}

async function loadLimitsOverview({ preserve = false } = {}) {
  const serial = ++state.requestSerial.limits;
  if (!preserve) {
    const loading = `<div class="empty-state">${escapeHTML(t("common.loading"))}</div>`;
    $("#limitsChart").innerHTML = loading;
    $("#limitsSummary").innerHTML = loading;
  }
  try {
    const [data, ...list] = await Promise.all([
      api(state.limitsDays === "custom" && state.limitsCustom
        ? `/api/v1/limits/series?since=${encodeURIComponent(state.limitsCustom.since)}&until=${encodeURIComponent(state.limitsCustom.until)}`
        : `/api/v1/limits/series?days=${state.limitsDays}`),
      ...LIMIT_PROVIDERS.map((provider) => api(`/api/v1/limits?provider=${provider}`))
    ]);
    if (serial !== state.requestSerial.limits) return;
    const payloads = Object.fromEntries(LIMIT_PROVIDERS.map((provider, index) => [provider, list[index]]));
    state.limitsOverview = { data, payloads };
    renderLimitsChart(data, payloads);
    renderLimitsSummary(payloads);
  } catch (error) {
    if (serial !== state.requestSerial.limits || preserve) return;
    $("#limitsChart").innerHTML = `<div class="empty-state">${escapeHTML(t("limits.loadError", { error: error.message }))}</div>`;
    toast(t("limits.loadError", { error: error.message }), true);
  } finally {
    if (serial === state.requestSerial.limits) scheduleLimitsRefresh();
  }
}

async function loadLimits({ preserve = false } = {}) {
  clearTimeout(limitsRefreshTimer);
  const provider = state.provider;
  const overview = provider === "all";
  $("#limitsOverview").hidden = !overview;
  $("#limitsSourcePane").hidden = overview;
  $("#limitsRange").hidden = !overview;
  $(".limits-account").hidden = overview;
  if (overview) return loadLimitsOverview({ preserve });
  const serial = ++state.requestSerial.limits;
  renderLimitsNote(provider);
  const keepVisible = preserve && state.limits?.provider === provider;
  if (!keepVisible) {
    const loading = `<div class="empty-state">${escapeHTML(t("common.loading"))}</div>`;
    $("#limitsStatus").innerHTML = loading;
    $("#limitsCurrent").innerHTML = loading;
    $("#limitsHistory").innerHTML = loading;
    $("#limitsCurrentMeta").textContent = "";
    $("#limitsHistoryMeta").textContent = "";
  }
  try {
    const payload = await api(`/api/v1/limits?provider=${encodeURIComponent(provider)}`);
    if (serial !== state.requestSerial.limits) return;
    state.limits = payload;
    renderLimits();
  } catch (error) {
    if (serial !== state.requestSerial.limits) return;
    // Background refreshes keep the last reading on screen and stay quiet, like status polling.
    if (!keepVisible) {
      const message = `<div class="empty-state">${escapeHTML(t("limits.loadError", { error: error.message }))}</div>`;
      $("#limitsStatus").innerHTML = message;
      $("#limitsCurrent").innerHTML = "";
      $("#limitsHistory").innerHTML = "";
      toast(t("limits.loadError", { error: error.message }), true);
    }
  } finally {
    if (serial === state.requestSerial.limits) scheduleLimitsRefresh();
  }
}

async function setClaudeLimits(enabled, button) {
  button.disabled = true;
  try {
    const result = await api("/api/v1/limits/claude", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled })
    });
    toast(t(result?.claude_enabled ? "limits.claude.enabledToast" : "limits.claude.disabledToast"));
    await loadLimits({ preserve: true });
    // The first reading arrives shortly after enabling, so look again sooner than a minute.
    if (result?.claude_enabled) scheduleLimitsRefresh(5000);
  } catch (error) {
    button.disabled = false;
    toast(t("limits.claude.toggleError", { error: error.message }), true);
  }
}

async function loadCurrentView(options = {}) {
  if (state.view === "daily") return loadDaily(options);
  if (state.view === "details") return loadDetails(options);
  if (state.view === "limits") return loadLimits(options);
  return loadOverview(options);
}

function switchView(next) {
  if (!next || next === state.view) return;
  const currentPanel = $(`[data-view-panel="${state.view}"]`);
  const nextPanel = $(`[data-view-panel="${next}"]`);
  const preserve = Boolean(state.viewVisited[next]);
  state.view = next;
  document.documentElement.dataset.view = next;
  if (next !== "limits") clearTimeout(limitsRefreshTimer);
  renderProviderTabs();
  $$('.nav-tab').forEach((tab) => {
    const selected = tab.dataset.view === next;
    tab.classList.toggle("active", selected);
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
  });
  history.replaceState(null, "", `#${next}`);
  currentPanel.hidden = true;
  currentPanel.classList.remove("active", "entering");
  nextPanel.hidden = false;
  nextPanel.classList.add("active", "entering");
  state.viewVisited[next] = true;
  loadCurrentView({ preserve });
  requestAnimationFrame(() => setTimeout(() => nextPanel.classList.remove("entering"), reducedMotion() ? 0 : 140));
}

function openDialog(dialog) {
  if (dialog.open) return;
  dialog.showModal();
  requestAnimationFrame(() => dialog.classList.add("is-open"));
}

function closeDialog(dialog) {
  if (!dialog?.open || dialog.classList.contains("is-closing")) return;
  dialog.classList.remove("is-open");
  dialog.classList.add("is-closing");
  setTimeout(() => {
    dialog.close();
    dialog.classList.remove("is-closing");
  }, reducedMotion() ? 0 : 200);
}

async function loadWarnings() {
  const container = $("#warningList");
  container.innerHTML = `<div class="empty-state">${escapeHTML(t("common.loading"))}</div>`;
  try {
    const payload = await api("/api/v1/warnings?limit=200");
    const items = payload.items || [];
    container.innerHTML = items.length ? items.map((item) => {
      const occurrences = Number(item.occurrences || 1);
      const firstSeen = item.first_seen || item.created_at;
      const period = occurrences > 1 ? t("warning.firstSeenMany", { time: localTime(firstSeen), count: fullToken(occurrences) }) : t("warning.firstSeen");
      const key = `warning.${item.kind}`;
      const label = t(key) === key ? item.kind : t(key);
      return `<article class="warning-row"><time>${escapeHTML(t("warning.recent", { time: localTime(item.created_at) }))}<small>${escapeHTML(period)}</small></time><code title="${escapeHTML(item.kind)}">${escapeHTML(label)}</code><details><summary>${escapeHTML(t("warning.raw"))}</summary><p title="${escapeHTML(item.path || "")}">${escapeHTML(serverText(item.detail))}</p></details></article>`;
    }).join("") : `<div class="empty-state">${escapeHTML(t("warning.none"))}</div>`;
  } catch (error) {
    container.innerHTML = `<div class="empty-state">${escapeHTML(error.message)}</div>`;
  }
}

function setExportLinks() {
  const bounds = currentBounds();
  const csvPath = apiURL("/api/v1/export", { ...bounds, format: "csv" });
  const jsonPath = apiURL("/api/v1/export", { ...bounds, format: "json" });
  $("#exportCsv").href = window.TokenUsageDemo?.createExportURL?.("csv", csvPath) || csvPath;
  $("#exportJson").href = window.TokenUsageDemo?.createExportURL?.("json", jsonPath) || jsonPath;
}

async function openPricingDialog() {
  openDialog($("#pricingDialog"));
  await loadPricing(state.provider === "all" ? "codex" : state.provider);
}

async function loadPricing(provider) {
  state.pricingProvider = provider;
  for (const tab of $$("[data-pricing-provider]")) {
    const selected = tab.dataset.pricingProvider === provider;
    tab.classList.toggle("selected", selected);
    tab.setAttribute("aria-selected", String(selected));
  }
  const ledger = provider !== "codex";
  $("#pricingIntro").textContent = t(ledger ? `pricing.intro.${provider}` : "pricing.intro");
  $("#pricingMeta").textContent = t("pricing.loading");
  $("#pricingNotes").hidden = true;
  $("#pricingOverrideList").innerHTML = `<div class="empty-state">${escapeHTML(t("pricing.loading"))}</div>`;
  $("#pricingCatalog").innerHTML = "";
  state.pricing = null;
  try {
    const payload = await api(`/api/v1/pricing${ledger ? `?provider=${encodeURIComponent(provider)}` : ""}`);
    if (state.pricingProvider !== provider) return;
    state.pricing = payload;
    if (ledger) renderLedgerPricing(payload);
    else renderPricing(payload);
  } catch (error) {
    $("#pricingOverrideList").innerHTML = `<div class="empty-state">${escapeHTML(error.message)}</div>`;
  }
}

function renderLedgerPricing(payload) {
  const unpriced = payload.unpriced_models || [];
  $("#pricingMeta").textContent = t("pricing.ledgerMeta", {
    currency: payload.currency, count: i18n.formatNumber(unpriced.length), tokens: fullToken(payload.unpriced_tokens || 0)
  });
  const notes = $("#pricingNotes");
  notes.innerHTML = (payload.notes || []).map((key) => `<li>${escapeHTML(t(`pricing.note.${key}`))}</li>`).join("");
  notes.hidden = !notes.children.length;
  renderLedgerCatalog(payload);
  const list = $("#pricingOverrideList");
  list.innerHTML = "";
  for (const item of unpriced) {
    const card = document.createElement("article");
    card.className = "override-card unpriced-card";
    card.innerHTML = `<div class="override-card-head"><div><strong>${escapeHTML(item.model || t("pricing.unknownName"))}</strong><small>${escapeHTML(t("pricing.unpricedRequests", { requests: i18n.formatNumber(item.requests), tokens: fullToken(item.tokens) }))}</small></div>${item.model && item.reason_kind === "unverified_rate" ? `<button class="text-button copper pressable" type="button">${escapeHTML(t("pricing.setRate"))}</button>` : ""}</div><p class="override-hint">${escapeHTML(reasonText(item.reason_kind, item.reason))}</p>`;
    $("button", card)?.addEventListener("click", () => appendLedgerOverrideCard({ model: item.model }));
    list.appendChild(card);
  }
  for (const row of payload.overrides || []) appendLedgerOverrideCard(row);
  if (!list.children.length) list.innerHTML = `<div class="empty-state">${escapeHTML(t("pricing.ledgerEmpty"))}</div>`;
}

function renderLedgerCatalog(payload) {
  const separate = payload.separate_1h_cache_write;
  const columns = separate ? "ledger-1h" : "ledger";
  const tier = (entry) => entry.min_prompt_tokens > 0 ? t("pricing.tier", { tokens: i18n.formatNumber(entry.min_prompt_tokens - 1) }) : "";
  const head = `<div class="catalog-row header ${columns}"><span>${escapeHTML(t("pricing.catalogModel"))}</span><span>${escapeHTML(t("pricing.validFrom"))}</span><span>Input</span><span>Cached</span><span>${separate ? "Write 5m" : "Write"}</span>${separate ? "<span>Write 1h</span>" : ""}<span>Output</span></div>`;
  const rows = (payload.catalog || []).map((entry) => `<div class="catalog-row ${columns}"><span><a href="${escapeHTML(entry.source)}" target="_blank" rel="noreferrer">${escapeHTML(entry.model)}</a>${tier(entry) ? `<small>${escapeHTML(tier(entry))}</small>` : ""}</span><span>${escapeHTML(entry.valid_from || t("pricing.sinceLaunch"))}</span><span>${entry.input}</span><span>${entry.cache_read}</span><span>${entry.cache_write || "—"}</span>${separate ? `<span>${entry.cache_write_1h ?? "—"}</span>` : ""}<span>${entry.output}</span></div>`).join("");
  $("#pricingCatalog").innerHTML = head + rows;
}

function appendLedgerOverrideCard(row) {
  const list = $("#pricingOverrideList");
  if ($(".empty-state", list)) list.innerHTML = "";
  const separate = state.pricing?.separate_1h_cache_write;
  const value = (key) => row[key] == null ? "" : escapeHTML(String(row[key]));
  const card = document.createElement("article");
  card.className = "override-card";
  card.dataset.ledgerOverride = "";
  card.innerHTML = `<div class="override-card-head"><div><input class="override-model" data-field="model" value="${value("model")}" placeholder="${escapeHTML(t("pricing.placeholder"))}" aria-label="${escapeHTML(t("pricing.catalogModel"))}"><small>${escapeHTML(t("pricing.localCustom"))}</small></div><button class="remove-override pressable" type="button">${escapeHTML(t("action.remove"))}</button></div>
    <div class="custom-rate-grid">
      <label>${escapeHTML(t("pricing.validFrom"))}<input type="date" data-field="valid_from" value="${value("valid_from")}"></label>
      <label>${escapeHTML(t("pricing.validTo"))}<input type="date" data-field="valid_to" value="${value("valid_to")}"></label>
      <label>${escapeHTML(t("pricing.minPrompt"))}<input data-field="min_prompt_tokens" inputmode="numeric" value="${value("min_prompt_tokens")}" placeholder="0"></label>
      <span></span>
      <label>Input<input data-rate="input" inputmode="decimal" value="${value("input")}" placeholder="USD / 1M"></label>
      <label>Cached<input data-rate="cache_read" inputmode="decimal" value="${value("cache_read")}" placeholder="USD / 1M"></label>
      <label>${separate ? "Write 5m" : "Cache Write"}<input data-rate="cache_write" inputmode="decimal" value="${value("cache_write")}" placeholder="USD / 1M"></label>
      ${separate ? `<label>Write 1h<input data-rate="cache_write_1h" inputmode="decimal" value="${value("cache_write_1h")}" placeholder="USD / 1M"></label>` : ""}
      <label>Output<input data-rate="output" inputmode="decimal" value="${value("output")}" placeholder="USD / 1M"></label>
    </div>
    <p class="override-hint">${escapeHTML(t("pricing.ledgerRateHint"))}</p>`;
  list.appendChild(card);
  $(".remove-override", card).addEventListener("click", () => card.remove());
  $("[data-field=model]", card).focus();
}

function collectLedgerOverrides() {
  const decimal = /^\d+(?:\.\d{1,4})?$/;
  return $$("[data-ledger-override]", $("#pricingOverrideList")).map((card) => {
    const separate = Boolean($("[data-rate=cache_write_1h]", card));
    const field = (name) => $(`[data-field=${name}]`, card).value.trim();
    const model = field("model");
    if (!model) throw new Error(t("pricing.modelRequired"));
    const validFrom = field("valid_from");
    if (!validFrom) throw new Error(t("pricing.validFromMissing", { model }));
    const row = { model, valid_from: validFrom, valid_to: field("valid_to") || null, min_prompt_tokens: 0 };
    const minPrompt = field("min_prompt_tokens");
    if (minPrompt) {
      if (!/^\d+$/.test(minPrompt)) throw new Error(t("pricing.minPromptInvalid", { model }));
      row.min_prompt_tokens = Number(minPrompt);
    }
    const required = ["input", "cache_read", "cache_write", "output", ...(separate ? ["cache_write_1h"] : [])];
    for (const key of required) {
      const rate = $(`[data-rate=${key}]`, card).value.trim();
      if (!rate) throw new Error(t("pricing.rateMissing", { model, rate: key }));
      if (!decimal.test(rate)) throw new Error(t("pricing.rateInvalid", { model }));
      row[key] = Number(rate);
    }
    return row;
  });
}

function renderPricing(payload) {
  const unpriced = payload.unpriced_models || [];
  $("#pricingMeta").textContent = t("pricing.meta", { currency: payload.currency, date: payload.catalog_as_of, count: i18n.formatNumber(unpriced.length) });
  renderCatalog(payload.catalog || []);
  const overrides = payload.overrides || {};
  const models = new Set([...unpriced.map((item) => item.key), ...Object.keys(overrides)]);
  const observed = new Map(unpriced.map((item) => [item.key, usageTotal(item.usage)]));
  const list = $("#pricingOverrideList");
  list.innerHTML = "";
  for (const model of models) {
    if (!model) continue;
    if (model === "未知") {
      const tokens = observed.get(model) || 0;
      list.insertAdjacentHTML("beforeend", `<article class="override-card"><div class="override-card-head"><div><strong>${escapeHTML(t("pricing.unknownName"))}</strong><small>${escapeHTML(t("pricing.observed", { tokens: fullToken(tokens) }))}</small></div></div><p class="override-hint">${escapeHTML(t("pricing.unknownHint"))}</p></article>`);
      continue;
    }
    appendOverrideCard(model, overrides[model], observed.get(model));
  }
  if (!list.children.length) list.innerHTML = `<div class="empty-state">${escapeHTML(t("pricing.empty"))}</div>`;
}

function renderCatalog(catalog) {
  $("#pricingCatalog").innerHTML = `<div class="catalog-row header"><span>${escapeHTML(t("pricing.catalogModel"))}</span><span>Input</span><span>Cached</span><span>Write</span><span>Output</span></div>${catalog.map((entry) => `<div class="catalog-row">${entry.source ? `<a href="${escapeHTML(entry.source)}" target="_blank" rel="noreferrer">${escapeHTML(entry.display_name)}</a>` : `<span>${escapeHTML(entry.display_name)}</span>`}<span>${escapeHTML(entry.input_usd_per_million)}</span><span>${escapeHTML(entry.cached_input_usd_per_million)}</span><span>${escapeHTML(entry.cache_write_input_usd_per_million || "—")}</span><span>${escapeHTML(entry.output_usd_per_million)}</span></div>`).join("")}`;
}

function appendOverrideCard(model, override = null, observedTokens = null) {
  const list = $("#pricingOverrideList");
  if ($('.empty-state', list)) list.innerHTML = "";
  if ($$('[data-pricing-model]', list).some((card) => card.dataset.pricingModel.toLowerCase() === model.toLowerCase())) {
    toast(t("pricing.duplicate"), true);
    return;
  }
  const mode = override?.alias_of ? "alias" : override ? "custom" : "unpriced";
  const aliasOptions = (state.pricing?.catalog || []).map((entry) => `<option value="${escapeHTML(entry.model)}" ${override?.alias_of === entry.model ? "selected" : ""}>${escapeHTML(entry.display_name)}</option>`).join("");
  const card = document.createElement("article");
  card.className = "override-card";
  card.dataset.pricingModel = model;
  card.innerHTML = `<div class="override-card-head"><div><strong>${escapeHTML(model)}</strong><small>${observedTokens == null ? escapeHTML(t("pricing.localCustom")) : escapeHTML(t("pricing.observed", { tokens: fullToken(observedTokens) }))}</small></div><button class="remove-override pressable" type="button">${escapeHTML(t("action.remove"))}</button></div>
    <div class="override-mode">
      <label>${escapeHTML(t("pricing.mode"))}<select data-rate-mode><option value="unpriced" ${mode === "unpriced" ? "selected" : ""}>${escapeHTML(t(model.toLowerCase() === "codex-auto-review" ? "pricing.defaultZero" : "pricing.keepUnpriced"))}</option><option value="alias" ${mode === "alias" ? "selected" : ""}>${escapeHTML(t("pricing.alias"))}</option><option value="custom" ${mode === "custom" ? "selected" : ""}>${escapeHTML(t("pricing.custom"))}</option></select></label>
      <label class="alias-field">${escapeHTML(t("pricing.publicModel"))}<select data-alias><option value="">${escapeHTML(t("pricing.chooseModel"))}</option>${aliasOptions}</select></label>
      <div class="custom-rate-grid">
        <label>Input<input data-rate="input_usd_per_million" inputmode="decimal" value="${escapeHTML(override?.input_usd_per_million || "")}" placeholder="USD / 1M"></label>
        <label>Cached<input data-rate="cached_input_usd_per_million" inputmode="decimal" value="${escapeHTML(override?.cached_input_usd_per_million || "")}" placeholder="USD / 1M"></label>
        <label>Cache Write<input data-rate="cache_write_input_usd_per_million" inputmode="decimal" value="${escapeHTML(override?.cache_write_input_usd_per_million || "")}" placeholder="USD / 1M"></label>
        <label>Output<input data-rate="output_usd_per_million" inputmode="decimal" value="${escapeHTML(override?.output_usd_per_million || "")}" placeholder="USD / 1M"></label>
      </div>
      <p class="override-hint">${escapeHTML(t("pricing.rateHint"))}</p>
    </div>`;
  list.appendChild(card);
  const updateMode = () => {
    const value = $('[data-rate-mode]', card).value;
    $('.alias-field', card).classList.toggle("hidden", value !== "alias");
    $('.custom-rate-grid', card).classList.toggle("hidden", value !== "custom");
    $('.override-hint', card).classList.toggle("hidden", value === "unpriced");
  };
  $('[data-rate-mode]', card).addEventListener("change", updateMode);
  $('.remove-override', card).addEventListener("click", () => card.remove());
  updateMode();
}

function collectPricingOverrides() {
  const overrides = {};
  const decimal = /^\d+(?:\.\d{1,3})?$/;
  for (const card of $$('[data-pricing-model]', $("#pricingOverrideList"))) {
    const model = card.dataset.pricingModel.trim();
    const mode = $('[data-rate-mode]', card).value;
    if (mode === "unpriced") continue;
    if (mode === "alias") {
      const alias = $('[data-alias]', card).value;
      if (!alias) throw new Error(t("pricing.aliasMissing", { model }));
      overrides[model] = { alias_of: alias };
      continue;
    }
    const value = {};
    for (const input of $$('[data-rate]', card)) {
      const rate = input.value.trim();
      if (rate) value[input.dataset.rate] = rate;
    }
    for (const key of ["input_usd_per_million", "cached_input_usd_per_million", "cache_write_input_usd_per_million", "output_usd_per_million"]) {
      if (!value[key]) throw new Error(t("pricing.rateMissing", { model, rate: key.replace("_usd_per_million", "") }));
    }
    for (const rate of Object.values(value)) if (!decimal.test(rate)) throw new Error(t("pricing.rateInvalid", { model }));
    overrides[model] = value;
  }
  return overrides;
}

async function savePricing() {
  const provider = state.pricingProvider || "codex";
  const ledger = provider !== "codex";
  let overrides;
  try { overrides = ledger ? collectLedgerOverrides() : collectPricingOverrides(); } catch (error) { toast(error.message, true); return; }
  const button = $("#savePricing");
  button.disabled = true;
  try {
    state.pricing = await api(`/api/v1/pricing/overrides${ledger ? `?provider=${encodeURIComponent(provider)}` : ""}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ overrides })
    });
    invalidateDataCache();
    toast(t("pricing.saved"));
    closeDialog($("#pricingDialog"));
    await loadCurrentView();
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function setupDialogBehavior() {
  $$('dialog').forEach((dialog) => {
    dialog.addEventListener("cancel", (event) => { event.preventDefault(); closeDialog(dialog); });
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog && !dialog.classList.contains("sheet-dialog")) closeDialog(dialog);
    });
    $$('[data-close]', dialog).forEach((button) => button.addEventListener("click", () => closeDialog(dialog)));
  });
}

function setupPressFeedback() {
  let pressed = null;
  document.addEventListener("pointerdown", (event) => {
    const target = event.target.closest(".pressable");
    if (!target || target.disabled) return;
    pressed?.classList.remove("is-pressed");
    pressed = target;
    pressed.classList.add("is-pressed");
  }, { passive: true });
  const clear = () => { pressed?.classList.remove("is-pressed"); pressed = null; };
  window.addEventListener("pointerup", clear, { passive: true });
  window.addEventListener("pointercancel", clear, { passive: true });
}

function tablistKeydown(event, tabs, activate) {
  if (!tabs.includes(event.target)) return;
  const keys = ["ArrowLeft", "ArrowRight", "Home", "End"];
  if (!keys.includes(event.key)) return;
  event.preventDefault();
  const current = Math.max(0, tabs.indexOf(event.target));
  const next = event.key === "Home" ? 0
    : event.key === "End" ? tabs.length - 1
      : (current + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
  tabs[next].focus();
  activate(tabs[next]);
}

function initialProvider() {
  const requested = new URLSearchParams(location.search).get("provider");
  if (PROVIDERS.includes(requested)) return requested;
  const saved = readStorage(PROVIDER_KEY);
  return PROVIDERS.includes(saved) ? saved : "all";
}

function renderProviderTabs() {
  $$('.provider-tab').forEach((tab) => {
    const selected = tab.dataset.provider === state.provider;
    tab.classList.toggle("active", selected);
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
  });
  document.documentElement.dataset.provider = state.provider;
}

// Each source has its own models, sources and sessions, so switching clears
// source-specific filters and every cached response before reloading.
function switchProvider(next) {
  if (!PROVIDERS.includes(next) || next === state.provider) return;
  state.provider = next;
  writeStorage(PROVIDER_KEY, next);
  const url = new URL(location.href);
  if (next === "all") url.searchParams.delete("provider"); else url.searchParams.set("provider", next);
  history.replaceState(null, "", url);
  renderProviderTabs();
  invalidateDataCache();
  state.filterOptions = null;
  state.filters = {};
  state.sessionSearch = "";
  if ($("#sessionSearch")) $("#sessionSearch").value = "";
  state.treeOffset = 0;
  resetDataSelections();
  syncFilterForm();
  renderFilterChips();
  state.limitsAccount = null;
  state.limitsHistoryLimit = {};
  state.viewVisited = { overview: false, daily: false, details: false, limits: false, [state.view]: true };
  loadCurrentView();
}

function setupEvents() {
  $$('.provider-tab').forEach((button) => button.addEventListener("click", () => switchProvider(button.dataset.provider)));
  $("#providerSwitch").addEventListener("keydown", (event) => tablistKeydown(event, $$('.provider-tab'), (button) => switchProvider(button.dataset.provider)));
  $$("[data-session-view]").forEach(button=>button.addEventListener("click",()=>{state.sessionView=button.dataset.sessionView;state.treeOffset=0;$$("[data-session-view]").forEach(b=>{const selected=b===button;b.classList.toggle("selected",selected);b.setAttribute("aria-pressed",String(selected));});loadSessions();}));
  $("#treePrevious").addEventListener("click",()=>{state.treeOffset=Math.max(0,state.treeOffset-20);loadSessions();});
  $("#treeNext").addEventListener("click",()=>{state.treeOffset+=20;loadSessions();});
  setupPressFeedback();
  setupDialogBehavior();
  $$('.nav-tab').forEach((button) => button.addEventListener("click", () => switchView(button.dataset.view)));
  $(".primary-nav").addEventListener("keydown", (event) => tablistKeydown(event, $$('.nav-tab'), (button) => switchView(button.dataset.view)));
  $("#brandButton").addEventListener("click", () => switchView("overview"));
  $$("[data-limits-days]").forEach((button) => button.addEventListener("click", () => {
    const value = button.dataset.limitsDays;
    $$("[data-limits-days]").forEach((item) => item.classList.toggle("selected", item === button));
    if (value === "custom") return openLimitsRange();
    state.limitsDays = Number(value);
    $("#limitsRangeForm").hidden = true;
    loadLimits();
  }));
  $("#limitsRangeNow").addEventListener("click", () => { $("#limitsRangeEnd").value = measuredMinute(new Date()); });
  $("#limitsRangeForm").addEventListener("submit", (event) => {
    event.preventDefault();
    if (!$("#limitsRangeForm").reportValidity()) return;
    const since = $("#limitsRangeStart").value;
    const until = $("#limitsRangeEnd").value;
    const error = since >= until ? t("range.orderError") : "";
    $("#limitsRangeError").textContent = error;
    $("#limitsRangeError").hidden = !error;
    if (error) return $("#limitsRangeEnd").focus();
    state.limitsCustom = { since, until };
    state.limitsDays = "custom";
    loadLimits();
  });
  $("#limitsAccount").addEventListener("change", (event) => {
    const value = event.target.value;
    state.limitsAccount = value.startsWith("a:") ? value.slice(2) : null;
    state.limitsHistoryLimit = {};
    renderLimits();
  });
  $("#limitsHistory").addEventListener("click", (event) => {
    const button = event.target.closest("[data-limits-more]");
    if (!button) return;
    const kind = button.dataset.limitsMore;
    state.limitsHistoryLimit[kind] = (state.limitsHistoryLimit[kind] || LIMITS_HISTORY_PAGE) + LIMITS_HISTORY_PAGE;
    renderLimits();
  });
  $("#limitsStatus").addEventListener("click", (event) => {
    const button = event.target.closest("[data-claude-limits]");
    if (button && !button.disabled) setClaudeLimits(button.dataset.claudeLimits === "on", button);
  });
  $$('[data-trend-view]').forEach((button) => button.addEventListener("click", () => switchTrendView(button.dataset.trendView)));
  $("#usageTrendPanel").querySelector('[role="tablist"]').addEventListener("keydown", (event) => tablistKeydown(event, $$('[data-trend-view]'), (button) => switchTrendView(button.dataset.trendView)));
  $("#previousHourDay").addEventListener("click", () => navigateHourlyDate(dateKey(addDays(state.hourlyDate, -1)), -1));
  $("#nextHourDay").addEventListener("click", () => navigateHourlyDate(dateKey(addDays(state.hourlyDate, 1)), 1));
  $("#currentHourDay").addEventListener("click", () => navigateHourlyDate(todayKey(), 1));
  $("#hourlyDatePicker").addEventListener("change", (event) => navigateHourlyDate(event.target.value));
  enableNativeDatePicker($("#hourlyDatePicker"));
  $$('[data-overview-range]').forEach((button) => button.addEventListener("click", () => {
    state.overviewRange = button.dataset.overviewRange;
    if (state.overviewRange === "custom") openCustomRange();
    delete state.filters.date;
    resetDataSelections();
    renderFilterChips();
    syncFilterForm();
    loadOverview();
  }));
  $("#customRangeForm").addEventListener("submit", queryCustomRange);
  $("#customRangeForm").addEventListener("input", () => {
    state.customRangeError = "";
    syncCustomRangeControls();
  });
  $("#rangeNow").addEventListener("click", () => {
    $("#rangeEnd").value = measuredMinute(new Date());
    state.customRangeError = "";
    syncCustomRangeControls();
  });
  $$('[data-detail-range]').forEach((button) => button.addEventListener("click", () => {
    state.detailRange = button.dataset.detailRange;
    delete state.filters.date;
    resetDataSelections();
    renderFilterChips();
    syncFilterForm();
    loadDetails();
  }));
  $("#detailDatePicker").addEventListener("change", (event) => {
    const value = event.target.value;
    const parsed = dateFromKey(value);
    if (value && (Number.isNaN(parsed.getTime()) || dateKey(parsed) !== value || value > todayKey())) {
      syncRangeControls();
      return;
    }
    if (value) state.filters.date = value;
    else delete state.filters.date;
    resetDataSelections();
    renderFilterChips();
    syncFilterForm();
    loadDetails();
  });
  enableNativeDatePicker($("#detailDatePicker"));
  $$('[data-dimension]').forEach((button) => button.addEventListener("click", () => {
    state.detailDimension = button.dataset.dimension;
    $$('[data-dimension]').forEach((item) => {
      const selected = item === button;
      item.classList.toggle("selected", selected);
      item.setAttribute("aria-selected", String(selected));
      item.tabIndex = selected ? 0 : -1;
    });
    loadBreakdown();
  }));
  $(".dimension-tabs").addEventListener("keydown", (event) => tablistKeydown(event, $$('[data-dimension]'), (button) => button.click()));
  $("#previousMonth").addEventListener("click", () => {
    state.monthCursor = new Date(state.monthCursor.getFullYear(), state.monthCursor.getMonth() - 1, 1);
    state.selectedDate = "";
    loadDaily();
  });
  $("#nextMonth").addEventListener("click", () => {
    state.monthCursor = new Date(state.monthCursor.getFullYear(), state.monthCursor.getMonth() + 1, 1);
    state.selectedDate = "";
    loadDaily();
  });
  $("#dailyDatePicker").addEventListener("change", (event) => navigateDailyDate(event.target.value));
  enableNativeDatePicker($("#dailyDatePicker"));
  $("#currentDay").addEventListener("click", () => navigateDailyDate(todayKey()));
  $("#filterButton").addEventListener("click", () => {
    syncFilterForm();
    openDialog($("#filterSheet"));
    loadFilterOptions();
  });
  $("#applyFilters").addEventListener("click", applyFiltersFromForm);
  $("#filterForm").addEventListener("submit", (event) => { event.preventDefault(); applyFiltersFromForm(); });
  $("#resetFilters").addEventListener("click", () => {
    for (const meta of Object.values(FILTER_FIELDS)) $(meta.selector).value = "";
  });
  $("#clearFilters").addEventListener("click", clearAllFilters);
  $("#sessionSearch").addEventListener("input", () => {
    const value = $("#sessionSearch").value;
    $("#sessionSearchClear").classList.toggle("hidden", !value);
    clearTimeout(sessionSearchTimer);
    sessionSearchTimer = setTimeout(applySessionSearch, 250);
  });
  $("#sessionSearch").addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      applySessionSearch();
    }
  });
  $("#sessionSearch").addEventListener("search", applySessionSearch);
  $("#sessionSearchClear").addEventListener("click", () => {
    $("#sessionSearch").value = "";
    applySessionSearch();
    $("#sessionSearch").focus();
  });
  $("#settingsButton").addEventListener("click", () => { syncSettingsForm(); openDialog($("#settingsDialog")); });
  $("#settingsForm").addEventListener("change", (event) => {
    const input = event.target.closest('[data-setting]');
    if (!input?.checked) return;
    if (input.dataset.setting === "locale") {
      i18n.setLocale(input.value);
      return;
    }
    updateDisplayPreference(input.dataset.setting, input.value);
  });
  $("#resetSettings").addEventListener("click", resetDisplayPreferences);
  $("#warningButton").addEventListener("click", () => { openDialog($("#warningsDialog")); loadWarnings(); });
  $("#pricingButton").addEventListener("click", openPricingDialog);
  $("#addOverride").addEventListener("click", () => {
    const input = $("#newOverrideModel");
    const model = input.value.trim().toLowerCase();
    if (!model) { toast(t("pricing.modelRequired"), true); return; }
    if (state.pricingProvider && state.pricingProvider !== "codex") appendLedgerOverrideCard({ model });
    else appendOverrideCard(model);
    input.value = "";
  });
  for (const tab of $$("[data-pricing-provider]")) {
    tab.addEventListener("click", () => { if (tab.dataset.pricingProvider !== state.pricingProvider) loadPricing(tab.dataset.pricingProvider); });
  }
  $("#newOverrideModel").addEventListener("keydown", (event) => {
    if (event.key === "Enter") { event.preventDefault(); $("#addOverride").click(); }
  });
  $("#savePricing").addEventListener("click", savePricing);
  $("#exportButton").addEventListener("click", () => { setExportLinks(); openDialog($("#exportDialog")); });
  const runScan = async (rebuild = false) => {
    const button = $("#scanButton");
    const rebuildButton = $("#confirmRebuild");
    button.disabled = true;
    rebuildButton.disabled = true;
    $(".scan-icon").classList.add("spin");
    try {
      const refreshFilterOptions = Boolean(state.filterOptions);
      const result = await api("/api/v1/rescan", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ rebuild })
      });
      invalidateDataCache();
      state.filterOptions = null;
      toast(t("scan.complete", { inserted: i18n.formatNumber(result.events_inserted || 0), duplicates: i18n.formatNumber(result.duplicates || 0) }));
      await loadStatus();
      await Promise.all([refreshFilterOptions ? loadFilterOptions({ force: true }) : Promise.resolve(), loadCurrentView()]);
    } catch (error) {
      if (!rebuild && error.payload?.rebuild_required) {
        $("#rebuildDetail").textContent = serverText(error.payload.detail) || error.message;
        openDialog($("#rebuildDialog"));
      } else {
        toast(error.message, true);
      }
    } finally {
      button.disabled = false;
      rebuildButton.disabled = false;
      $(".scan-icon").classList.remove("spin");
    }
  };
  $("#scanButton").addEventListener("click", () => runScan(false));
  $("#confirmRebuild").addEventListener("click", async () => {
    closeDialog($("#rebuildDialog"));
    await runScan(true);
  });
  $("#themeButton").addEventListener("click", () => {
    const dark = displayPreferences.theme === "system"
      ? window.matchMedia("(prefers-color-scheme: dark)").matches
      : displayPreferences.theme === "dark";
    const next = dark ? "light" : "dark";
    updateDisplayPreference("theme", next);
  });
  $("#localeButton").addEventListener("click", () => {
    i18n.setLocale(i18n.getLocale() === "en" ? "zh-TW" : "en");
  });
  window.addEventListener("token-usage-locale-change", async () => {
    syncSettingsForm();
    renderFilterOptions();
    renderFilterChips();
    try {
      await Promise.all([loadStatus(), loadCurrentView({ preserve: false })]);
    } catch (error) {
      toast(error.message, true);
    }
  });
}

function scheduleStatusPoll(delay = document.visibilityState === "visible" ? VISIBLE_STATUS_POLL_MS : HIDDEN_STATUS_POLL_MS) {
  clearTimeout(statusPollTimer);
  statusPollTimer = setTimeout(pollStatus, delay);
}

async function pollStatus() {
  if (statusPollRunning) return scheduleStatusPoll();
  statusPollRunning = true;
  try {
    const changed = await loadStatus();
    if (changed && document.visibilityState !== "visible") state.pendingDataRefresh = true;
    if (document.visibilityState === "visible" && (changed || state.pendingDataRefresh)) {
      state.pendingDataRefresh = false;
      await loadCurrentView({ preserve: true });
    }
  } catch {} finally {
    statusPollRunning = false;
    scheduleStatusPoll();
  }
}

function startStatusPolling() {
  document.addEventListener("visibilitychange", () => {
    scheduleStatusPoll(document.visibilityState === "visible" ? 0 : HIDDEN_STATUS_POLL_MS);
    // Limits refresh only while the page is visible and catch up on return.
    if (state.view !== "limits") return;
    if (document.visibilityState === "visible") loadLimits({ preserve: true });
    else clearTimeout(limitsRefreshTimer);
  });
  scheduleStatusPoll();
}

async function boot() {
  applyDisplayPreferences();
  i18n.applyStatic();
  state.provider = initialProvider();
  renderProviderTabs();
  if (window.TOKEN_USAGE_DEMO || window.TokenUsageDemo) $("#demoBanner").classList.remove("hidden");
  setupEvents();
  renderFilterChips();
  const requestedView = location.hash.slice(1);
  if (["daily", "details", "limits"].includes(requestedView)) {
    const initial = state.view;
    state.view = requestedView;
    state.viewVisited[requestedView] = true;
    $(`[data-view-panel="${initial}"]`).hidden = true;
    $(`[data-view-panel="${requestedView}"]`).hidden = false;
    $$('.nav-tab').forEach((tab) => {
      const selected = tab.dataset.view === requestedView;
      tab.classList.toggle("active", selected);
      tab.setAttribute("aria-selected", String(selected));
      tab.tabIndex = selected ? 0 : -1;
    });
  }
  document.documentElement.dataset.view = state.view;
  renderProviderTabs();
  await loadStatus().catch((error) => toast(error.message, true));
  await loadCurrentView();
  startStatusPolling();
}

boot();
