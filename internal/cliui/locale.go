package cliui

import (
	"fmt"
	"strings"
)

type Locale string

const (
	Chinese Locale = "zh-TW"
	English Locale = "en"
)

var messages = map[Locale]map[string]string{
	Chinese: {
		"error.prefix":         "錯誤:",
		"error.invalidLang":    "不支援語言 %q；可選值為 en 或 zh-TW",
		"error.langMissing":    "--lang 需要一個值；可選值為 en 或 zh-TW",
		"error.unknownCommand": "未知命令 %q",
		"usage": `Token Usage — 目前電腦的 Codex、Claude Code 與 Antigravity Token 本機統計

用法:
  token-usage [--lang en|zh-TW]       開啟本機 Dashboard
  token-usage open                    開啟本機 Dashboard
  token-usage install                 使用者級安裝、初始化 JSONL 掃描並啟動
  token-usage summary --since 7d      命令列摘要（支援 --json / --csv）
  token-usage scan [--rebuild]        增量掃描本機 Codex、Claude Code 與 Antigravity 記錄
  token-usage doctor                  檢查路徑、JSONL 資料來源與服務狀態
  token-usage config add-home PATH    新增一個額外 CODEX_HOME
  token-usage uninstall [--purge]     解除安裝；預設保留所有資料
  token-usage serve                   前景執行本機服務
  token-usage version                 顯示版本

語言:
  --lang 優先於 TOKEN_USAGE_LANG，其次跟隨系統語言；支援 en、zh-TW（zh-CN 等其他中文代碼也視為 zh-TW）。

邊界:
  「電腦」是執行這些用戶端和 token-usage 的主機；不是 shell/tool 實際執行的遠端環境。
  統計用量時不讀取帳號憑證，不儲存回覆、reasoning 或工具輸出（Codex 對話標題與專案路徑除外）；成本是按本機 Token 與內建價格估算的 API 等價成本，並非帳單。
  額度頁會讀取各工具回報的用量額度與帳號身分（不含權杖），細節見 README。`,
		"serve.running":          "Token Usage 正在 %s 執行；按 Ctrl+C 停止。\n",
		"open.start":             "啟動本機服務: %w",
		"open.notReady":          "本機服務未在 12 秒內就緒；請執行 token-usage doctor",
		"open.opened":            "已開啟",
		"open.noGUI":             "無圖形環境時，在你的電腦執行：ssh -N -L %d:127.0.0.1:%d %s@%s\n",
		"flag.rebuild":           "清空衍生統計後從全部 JSONL 重建",
		"flag.json":              "輸出 JSON",
		"flag.csv":               "輸出 CSV",
		"flag.since":             "範圍：7d、30d、today、all 或 RFC3339",
		"flag.skipScan":          "跳過首次歷史掃描",
		"flag.purge":             "同時刪除整個服務資料夾（三個帳本、額度紀錄與設定）",
		"summary.conflict":       "--json 與 --csv 不能同時使用",
		"summary.title":          "Token Usage · Codex · 目前電腦 · %s\n",
		"summary.cached":         "  Cached Input    %s  (Input 的子集)\n",
		"summary.reasoning":      "  Reasoning       %s  (Output 的子集)\n",
		"summary.unattributed":   "歷史未歸屬        %s  (只屬於累計，不屬於所選日期)\n",
		"scan.complete":          "掃描完成：%d 個 Home，%d 個檔案，新增 %d 個事件，忽略 %d 個重複，%d 則提示，耗時 %.2fs\n",
		"scan.unattributed":      "有 %d 個 session 的差額僅計入「歷史未歸屬」，未偽造每日分佈。\n",
		"install.stopCurrent":    "停止現有背景服務: %w",
		"install.permissions":    "警告：無法進一步限縮服務資料夾權限:",
		"install.installed":      "已安裝:",
		"install.scanning":       "正在執行首次本機歷史掃描（只解析 metadata 與 token_count）…",
		"install.scanWarning":    "警告：首次掃描未完成:",
		"install.scanDone":       "首次掃描：%d 檔案，新增 %d 事件，%d 則提示。\n",
		"install.health":         "背景服務未在 30 秒內通過本機 health check",
		"install.service":        "背景服務:",
		"install.warning":        "警告:",
		"install.done":           "安裝完成。背景服務會定期增量掃描本機 JSONL；無需重啟 Codex。",
		"doctor.loopbackOK":      "%s:%d；不會監聽公開網路",
		"doctor.loopbackError":   "設定了非 loopback 位址",
		"doctor.permissions":     "服務資料夾已限制為目前使用者存取",
		"doctor.machineHost":     "資料庫最初屬於主機 %s，目前主機為 %s；可能複製/同步了 TOKEN_USAGE_HOME，逐電腦邊界不再可靠",
		"doctor.machinePlatform": "資料庫記錄平台為 %s/%s，目前為 %s/%s；請勿跨電腦同步 TOKEN_USAGE_HOME",
		"doctor.database":        "%s；events=%d sessions=%d",
		"doctor.jsonlOnly":       "JSONL 是唯一 Token 計數來源；狀態資料庫僅用於發現檔案與補充 metadata",
		"doctor.coverage":        "%d 則異常/覆蓋提示，請在 Dashboard 查看",
		"doctor.homeUnreadable":  "%s 不存在或不可讀",
		"doctor.stateWarning":    "%s：%s；掃描 %d 個 JSONL",
		"doctor.stateDB":         "%s：%d 個 canonical rollout",
		"doctor.stateMissing":    "%s 沒有可識別狀態資料庫；掃描 %d 個 JSONL",
		"doctor.sharedHistory":   "session %s 同時出現在 %s 與 %s。若多台電腦同步同一 CODEX_HOME，安裝前歷史無法可靠拆分；統計會按 session 去重。",
		"doctor.sharedHome":      "%s 位於常見跨系統掛載路徑；確認它不是與 Windows 共用的 CODEX_HOME",
		"doctor.serviceOK":       "本機 Dashboard 可存取",
		"doctor.serviceDown":     "本機服務未執行",
		"doctor.privacy":         "資料庫 schema 僅含計數、時間、模型、來源、路徑和標題；無 prompt/reply/reasoning/auth 欄位",
		"doctor.network":         "執行時無外部回報用戶端；Dashboard 僅監聽 loopback",
		"config.homeAdded":       "已新增額外 Codex Home:",
		"config.scanHint":        "執行 token-usage scan 開始統計。",
		"uninstall.portable":     "已停止服務並移除登入啟動；程式與 data 資料夾保留在:",
		"uninstall.purged":       "已解除安裝服務與程式，並刪除服務資料夾（三個帳本、額度紀錄與設定，無法從工具內恢復）。",
		"uninstall.kept":         "已解除安裝服務與程式；資料（三個帳本、額度紀錄與設定）保留在:",
		"uninstall.purgeHint":    "如需刪除資料，執行 token-usage uninstall --purge，會刪除整個服務資料夾。"},
	English: {
		"error.prefix":         "Error:",
		"error.invalidLang":    "unsupported language %q; use en or zh-TW",
		"error.langMissing":    "--lang requires a value; use en or zh-TW",
		"error.unknownCommand": "unknown command %q",
		"usage": `Token Usage — local Codex, Claude Code and Antigravity token accounting for this machine

Usage:
  token-usage [--lang en|zh-TW]       Open the local Dashboard
  token-usage open                    Open the local Dashboard
  token-usage install                 Install for this user, initialize JSONL scanning, and start
  token-usage summary --since 7d      Print a CLI summary (supports --json / --csv)
  token-usage scan [--rebuild]        Incrementally scan local Codex, Claude Code and Antigravity records
  token-usage doctor                  Check paths, JSONL sources, and service state
  token-usage config add-home PATH    Add another CODEX_HOME
  token-usage uninstall [--purge]     Uninstall; keep all data by default
  token-usage serve                   Run the local service in the foreground
  token-usage version                 Print the version

Language:
  --lang overrides TOKEN_USAGE_LANG, then the system locale; supports en and zh-TW (other zh codes map to zh-TW).

Boundary:
  “Machine” means the host running these clients and token-usage, not a remote shell/tool target.
  Usage accounting never reads account credentials and never stores replies, reasoning or tool output (Codex
  conversation titles and project paths excepted); cost is an API-equivalent estimate of local token usage, not a bill.
  The Limits page reads each tool's reported usage limits and account identity (no tokens); see the README.`,
		"serve.running":          "Token Usage is running at %s; press Ctrl+C to stop.\n",
		"open.start":             "start local service: %w",
		"open.notReady":          "the local service was not ready within 12 seconds; run token-usage doctor",
		"open.opened":            "Opened",
		"open.noGUI":             "With no graphical session, run this on your computer: ssh -N -L %d:127.0.0.1:%d %s@%s\n",
		"flag.rebuild":           "Clear derived accounting and rebuild it from all JSONL files",
		"flag.json":              "Output JSON",
		"flag.csv":               "Output CSV",
		"flag.since":             "Range: 7d, 30d, today, all, or RFC3339",
		"flag.skipScan":          "Skip the first historical scan",
		"flag.purge":             "Also delete the whole service folder (all three ledgers, the limit records and settings)",
		"summary.conflict":       "--json and --csv cannot be used together",
		"summary.title":          "Token Usage · Codex · this machine · %s\n",
		"summary.cached":         "  Cached Input    %s  (subset of Input)\n",
		"summary.reasoning":      "  Reasoning       %s  (subset of Output)\n",
		"summary.unattributed":   "Historical only   %s  (part of all-time totals, not the selected dates)\n",
		"scan.complete":          "Scan complete: %d Homes, %d files, %d events added, %d duplicates ignored, %d notices, %.2fs\n",
		"scan.unattributed":      "%d sessions have deltas counted only as historical unattributed usage; no daily distribution was invented.\n",
		"install.stopCurrent":    "stop the current background service: %w",
		"install.permissions":    "Warning: could not further restrict the service folder:",
		"install.installed":      "Installed:",
		"install.scanning":       "Running the first local history scan (metadata and token_count only)…",
		"install.scanWarning":    "Warning: first scan did not complete:",
		"install.scanDone":       "First scan: %d files, %d events added, %d notices.\n",
		"install.health":         "the background service failed its local health check within 30 seconds",
		"install.service":        "Background service:",
		"install.warning":        "Warning:",
		"install.done":           "Installation complete. The background service incrementally scans local JSONL files; Codex does not need to restart.",
		"doctor.loopbackOK":      "%s:%d; not exposed publicly",
		"doctor.loopbackError":   "a non-loopback address is configured",
		"doctor.permissions":     "service folder access is restricted to the current user",
		"doctor.machineHost":     "database originally belonged to host %s; current host is %s. TOKEN_USAGE_HOME may have been copied or synced, so the per-machine boundary is unreliable",
		"doctor.machinePlatform": "database platform is %s/%s; current platform is %s/%s. Do not sync TOKEN_USAGE_HOME across machines",
		"doctor.database":        "%s; events=%d sessions=%d",
		"doctor.jsonlOnly":       "JSONL is the only token-accounting source; the state database is used only for discovery and metadata",
		"doctor.coverage":        "%d anomaly/coverage notices; review them in the Dashboard",
		"doctor.homeUnreadable":  "%s does not exist or is unreadable",
		"doctor.stateWarning":    "%s: %s; scanning %d JSONL files",
		"doctor.stateDB":         "%s: %d canonical rollouts",
		"doctor.stateMissing":    "%s has no recognizable state database; scanning %d JSONL files",
		"doctor.sharedHistory":   "session %s appears in both %s and %s. If multiple machines share one CODEX_HOME, pre-install history cannot be separated reliably; sessions are deduplicated",
		"doctor.sharedHome":      "%s is on a common cross-system mount; confirm it is not shared with Windows",
		"doctor.serviceOK":       "local Dashboard is reachable",
		"doctor.serviceDown":     "local service is not running",
		"doctor.privacy":         "database schema contains counts, time, model, source, path, and title only; no prompt/reply/reasoning/auth fields",
		"doctor.network":         "runtime has no external reporting client; the Dashboard listens on loopback only",
		"config.homeAdded":       "Added another Codex home:",
		"config.scanHint":        "Run token-usage scan to start accounting.",
		"uninstall.portable":     "Stopped the service and removed the login start; the program and its data folder remain in:",
		"uninstall.purged":       "Uninstalled the service and program and deleted the service folder (all three ledgers, the limit records and settings; this cannot be undone from the tool).",
		"uninstall.kept":         "Uninstalled the service and program; data (all three ledgers, the limit records and settings) remains in:",
		"uninstall.purgeHint":    "To delete the data, run token-usage uninstall --purge; it deletes the whole service folder."},
}

func Normalize(value string) (Locale, bool) {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
	switch {
	case value == "en" || strings.HasPrefix(value, "en-"):
		return English, true
	case value == "zh" || strings.HasPrefix(value, "zh-"):
		return Chinese, true
	default:
		return "", false
	}
}

func Detect(explicit, environment, system string) (Locale, error) {
	if explicit != "" {
		locale, ok := Normalize(explicit)
		if !ok {
			return "", fmt.Errorf(messages[English]["error.invalidLang"], explicit)
		}
		return locale, nil
	}
	if locale, ok := Normalize(environment); ok {
		return locale, nil
	}
	if locale, ok := Normalize(system); ok {
		return locale, nil
	}
	return Chinese, nil
}

func (locale Locale) Text(key string, args ...any) string {
	if locale != English {
		locale = Chinese
	}
	format, ok := messages[locale][key]
	if !ok {
		format = messages[Chinese][key]
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

func Catalogs() map[Locale]map[string]string { return messages }
