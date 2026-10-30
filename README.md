# Token Usage

**中文** | [English](README.en.md)

在自己的電腦上統計 **Codex**、**Claude Code** 與 **Antigravity** 的 Token 用量與 API 等價成本。三個來源合在同一個本機網頁裡，可以看整體，也可以切換到單一來源。所有資料只留在這台電腦。

![Token Usage 概覽](docs/images/dashboard.png)

## 功能

- **來源切換**：頁首一排圖示按鈕，可在「整體 · Claude · Codex · Antigravity」之間切換。選擇會記在瀏覽器，也可以用網址參數 `?provider=claude` 直接開啟。
- **概覽**：今日、7 日、30 日、全部，或精確到分鐘的自訂區間。顯示總 Token、Input／Cached／Cache Write／Output／Reasoning 拆分、API 等價成本與定價覆蓋率，以及每小時與每日趨勢。
- **每日**：月曆熱度、點選日期看當日模型構成與費用。
- **明細**：依模型、來源、Agent、專案、Thread 分組；對話列表與任務樹；搜尋對話。
- **匯出**：依目前來源與篩選匯出 CSV 或 JSON。
- 繁體中文／English、淺色／深色主題、字級與密度設定。

![每日用量](docs/images/daily.png)

## 會統計什麼

| 來源 | 讀取的本機資料 | 記錄內容 |
|---|---|---|
| Codex | `CODEX_HOME`（預設 `~/.codex`）的 `sessions/`、`archived_sessions/` JSONL，以及 Codex 狀態資料庫的路徑與標題 | Token 分類、模型、來源、專案、Thread、Agent、Fast 模式 |
| Claude Code | `~/.claude/projects`、`~/.config/claude/projects` 下的 JSONL（含子代理與已同步到本機的 SSH 會話） | Token 分類、模型、5 分鐘／1 小時快取寫入、思考 Token、速度與推論地區 |
| Antigravity | `~/.gemini/antigravity*/brain/*/.system_generated/logs/transcript.jsonl` 與對應的 `conversations/*.db` | Token 分類、模型、思考 Token、模型選擇紀錄 |

**整體**就是三個來源相加。同一個模型若同時出現在 Claude Code 與 Antigravity（例如 `claude-opus-5-5`），在模型分組裡會合併成同一列。

不會保存回覆、思考內容或工具輸出。Claude Code 與 Antigravity 帳本只存 Token 計數、模型、時間與雜湊後的識別碼；Codex 另外保存 Codex 自己記錄的對話標題與專案路徑，而對話標題通常就是第一句提示詞。Antigravity 缺少原生計數時，會讀取文字計算字數來估算 Token，但同樣不保存內容。不會讀取帳號憑證（如 `auth.json`）、真實帳單或帳號配額。

詳細的去重、歸屬與計價規則見 [計量與計價說明](docs/accounting.md)。

## 快速開始

### 1. 建置

需要 Go 1.26 以上。

```bash
go build -o codex-usage.exe ./cmd/codex-usage
```

或執行 `scripts/build.ps1`（Windows）／`scripts/build.sh`（Linux、macOS），會先跑完整測試，再輸出各平台執行檔到 `dist/`。

### 2. 決定資料放哪裡

在執行檔旁邊建立一個 `data` 資料夾，程式就會以**可攜模式**執行：三個帳本都放在這個資料夾裡，程式和資料可以一起放進 OneDrive 等同步資料夾。沒有 `data` 資料夾時，資料放在使用者的應用程式資料夾（見下方「資料存在哪裡」）。

### 3. 安裝並啟動

```bash
codex-usage install
```

安裝會先完整掃描一次 Codex 紀錄，設定登入時自動在背景啟動並立即啟動服務；服務啟動後會接著掃描 Claude Code 與 Antigravity。不需要系統管理員權限。可攜模式下程式留在原地執行；一般模式會把程式複製到使用者資料夾。

### 4. 開啟

```bash
codex-usage
```

或直接開 **http://127.0.0.1:43189/**。

## 常用命令

```text
codex-usage                         開啟網頁
codex-usage summary --since 7d      近 7 日 Codex 摘要（可加 --json / --csv）
codex-usage scan                    增量掃描三個來源
codex-usage scan --rebuild          從現有 JSONL 重建 Codex 統計
codex-usage serve                   在前景執行本機服務
codex-usage doctor                  檢查 Codex 路徑、資料來源與服務狀態
codex-usage config add-home PATH    加入額外的 CODEX_HOME
codex-usage uninstall               停止服務、移除登入啟動（一般模式另刪程式），保留帳本
codex-usage uninstall --purge       一般模式：另外刪除 Codex 統計資料
```

背景服務啟動時會先掃描一次。之後 Codex 每 30 秒檢查檔案是否有變化，有變化才掃描，並每 10 分鐘保底掃描一次；Claude Code 與 Antigravity 每 10 分鐘掃描一次。網頁上的「掃描」按鈕會立即掃描三個來源。

**移除**：一般模式的 `uninstall` 會刪除複製到使用者資料夾的程式；`--purge` 另外刪除 Codex 統計資料，Claude Code 與 Antigravity 帳本一律保留，需要時請自行刪除。可攜模式的 `uninstall` 只停止服務並移除登入啟動，不刪除程式或 `data` 資料夾，也不接受 `--purge`。

## 資料存在哪裡

### 可攜模式（執行檔旁有 `data` 資料夾）

```text
Token Usage Tracker/
├─ codex-usage.exe
└─ data/
   ├─ codex/         Codex 統計（usage.sqlite）、設定（config.json）、備份
   ├─ claude/        Claude Code 帳本（usage.sqlite）
   └─ antigravity/   Antigravity 帳本（usage.sqlite）
```

放在 OneDrive 時請注意：

- 對 `data` 資料夾按右鍵選「一律保留在此裝置上」，避免檔案只留在雲端。
- 同一份 `data` 一次只讓一台電腦執行，不要兩台同時開著。
- 帳本是 SQLite 資料庫，執行中會有 `-wal`、`-shm` 附屬檔。雲端上的副本可能落後幾分鐘；要在另一台電腦使用時，先在原電腦停止服務，等同步完成再開。

### 一般模式

Windows 的預設位置：

| 內容 | 位置 |
|---|---|
| 程式 | `%LOCALAPPDATA%\Programs\codex-usage\codex-usage.exe` |
| Codex 統計與設定 | `%LOCALAPPDATA%\codex-usage\`（`usage.sqlite`、`config.json`） |
| Claude Code 帳本 | `%LOCALAPPDATA%\ClaudeUsageTracker\usage.sqlite` |
| Antigravity 帳本 | `%LOCALAPPDATA%\AntigravityTokenTracker\usage.sqlite` |

Linux 與 macOS 的 Codex 統計放在 `~/.local/share/codex-usage` 與 `~/Library/Application Support/codex-usage`；另外兩個帳本放在系統快取資料夾下的同名資料夾。

三個帳本都是只增不刪：原始 JSONL 被清掉或移走之後，已經記錄的用量仍然保留。帳目若因來源修正而變動，舊值會留在帳本的修訂紀錄表中。

### 設定與環境變數

`config.json`：

```json
{
  "listen_address": "127.0.0.1",
  "port": 43189,
  "scan_interval_seconds": 600
}
```

| 變數 | 用途 |
|---|---|
| `CODEX_HOME` | Codex 資料夾（預設 `~/.codex`） |
| `CODEX_USAGE_HOME` | Codex 統計資料夾，必須是專用的空資料夾 |
| `CODEX_USAGE_TIMEZONE` | 首次建立統計庫時使用的計量時區，例如 `Asia/Taipei` |
| `CODEX_USAGE_LANG` | 命令列語言：`en` 或 `zh-TW` |
| `CLAUDE_CONFIG_DIR` | Claude Code 設定資料夾，可用逗號分隔多個 |
| `CLAUDE_TRACKER_DATA` | Claude Code 帳本資料夾 |
| `ANTIGRAVITY_TRACKER_HOME` | Antigravity 資料夾（預設 `~/.gemini`） |
| `ANTIGRAVITY_TRACKER_DATA` | Antigravity 帳本資料夾 |

## 隱私與安全

- 服務只綁定 `127.0.0.1`，拒絕綁定其他位址；寫入類 API 會檢查來源必須是本機。
- 網頁不載入任何外部資源，有嚴格的 Content-Security-Policy。
- 不上傳任何資料、沒有遙測、不會自動連網下載或更新。
- 價格表隨程式內建，執行時不會抓取網頁。

## 費用的意義

金額是依各家公開 **Standard API 價格**換算的等價成本，**不是**你的訂閱扣款或實際帳單：

- Codex 使用內建 OpenAI 價格表；明確標記為 Fast 的用量按 Codex 額度倍率折算。
- Claude Code 依 Anthropic 官方日期費率，區分 5 分鐘與 1 小時快取寫入；快速模式與美國推論地區按官方倍率。
- Antigravity 依各模型的日期費率，Gemini 3.1 Pro 長上下文使用較高費率。

沒有已核實費率的模型或日期會標為「未定價」，Token 照常統計，不會被當成零元。可以在「定價設定」為 Codex 模型指定別名或自訂單價。

## 已知限制

- 只統計這台電腦上的紀錄；其他電腦的用量要在那台電腦上看。
- 原始紀錄在安裝前就已刪除的部分，無法補回。
- 只有 Codex 的紀錄含專案與對話標題；Claude Code 與 Antigravity 的對話以「來源 · ID 前 8 碼」顯示。
- 「定價設定」視窗目前只能編輯 Codex 的費率。
- `summary` 命令目前只輸出 Codex 的摘要，三個來源合計請看網頁或匯出。

## 開發與測試

```bash
go test ./...
go vet ./...
```

網頁原始檔在 `internal/web/static/`，建置時會嵌入執行檔。

## 來源與授權

本專案修改自 zJay 的 [codex-usage](https://github.com/zJay26/codex-usage)。

以 GNU 通用公共授權條款第 3 版（GPLv3）發布，見 [LICENSE](LICENSE)；上游專案的 MIT 授權聲明與相依套件授權見 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
