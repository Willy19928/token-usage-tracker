# 開發說明

## 環境

- Go 1.26 以上
- 不需要 Node.js 或其他執行環境；網頁檔案直接嵌入執行檔。

## 專案結構

| 路徑 | 內容 |
|---|---|
| `cmd/token-usage` | 程式進入點 |
| `internal/app` | 命令列、安裝、背景服務與排程掃描 |
| `internal/usage` | Codex JSONL 掃描與去重 |
| `internal/store` | Codex 統計資料庫與查詢 |
| `internal/ledger` | Claude Code 與 Antigravity 帳本：掃描、去重、計價與查詢 |
| `internal/pricing` | 費用估算（Codex 價格表與外部已計價事件的彙總） |
| `internal/server` | 本機 HTTP API 與來源切換（`provider` 參數） |
| `internal/web/static` | 網頁（HTML、CSS、JavaScript、圖示） |
| `cmd/token-usage/winres` | Windows 程式圖示（`icon.ico`）；`rsrc_windows_*.syso` 由它產生 |
| `scripts/winicon` | 把 `icon.ico` 轉成 Go 連結時嵌入的 Windows 圖示資源 |

## 常用指令

```bash
go test ./...
go vet ./...
go build -o token-usage.exe ./cmd/token-usage
```

在前景用另一組資料試跑，不影響正式資料：

```bash
TOKEN_USAGE_HOME=/path/to/empty-dir \
CLAUDE_TRACKER_DATA=/path/to/claude-copy \
ANTIGRAVITY_TRACKER_DATA=/path/to/ag-copy \
token-usage serve
```

`TOKEN_USAGE_HOME` 必須是專用資料夾；可在其中的 `config.json` 改用其他連接埠。

更換程式圖示：替換 `cmd/token-usage/winres/icon.ico`（多尺寸，含 16 到 256 px），再執行下面的指令重新產生 `.syso`，並一併提交：

```bash
go run ./scripts/winicon -ico cmd/token-usage/winres/icon.ico -out cmd/token-usage
```

## 修改原則

- 計量規則的變更要附上測試，並確認既有帳目在重新掃描後不變。
- 不新增任何對外連線、遙測或外部資源。
- 網頁文字同時更新 `i18n.js` 的繁體中文與英文。
- 新的價格必須來自官方公開價格，並註明生效日；不要把新價格套用到生效日之前。
