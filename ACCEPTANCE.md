# 驗收紀錄

## v1.0.1（2026-10-31）

- Claude Code 補上 Fable 5.1、Fable 5、Opus 5、Opus 4.8、Opus 4.7、Sonnet 5、Sonnet 4.6、Haiku 4.5 的官方 Standard API 費率；Opus 5 與 Opus 4.8 的快速模式以 2 倍計價。
- Claude Code 在本機產生、未呼叫 API 的訊息（模型標示為 `<synthetic>`，例如用量上限提示）不再列入統計。
- `go test ./...`、`go vet ./...`、`gofmt -l .` 通過。

## v1.0.0（2026-10-30）

本版把 Codex、Claude Code 與 Antigravity 三個來源合併到同一個程式與網頁，加入來源切換，並支援把帳本放在執行檔旁 `data` 資料夾的可攜模式。

### 自動化檢查

- `go test ./...`：全部套件通過，包含 `internal/ledger` 的去重、計價、刪除來源後保留歷史、重複掃描不變、Antigravity 計數與估算、protobuf 解析等測試。
- `go vet ./...`、`gofmt -l .`：無輸出。

驗收時另外發現並修正一個問題：Claude Code 紀錄缺少速度、推論地區或服務層級欄位時，原本被當成「未知速度」而不計價；現在比照官方預設，視為 standard／global。

### 網頁

- 整體、Claude、Codex、Antigravity 四種來源都能切換；網址參數 `?provider=` 與瀏覽器記憶正常。
- 概覽、每日、明細三個頁面在各來源下皆正常顯示，瀏覽器主控台沒有錯誤。
- 明細的對話列表混合三個來源，各自以自己的規則計價。
- 頁面上沒有軟體更新、檢查更新或 GitHub 連結；唯一的外部連結是「定價設定」裡各模型的官方價格頁。
- 可攜模式：執行檔旁有 `data` 資料夾時，三個帳本與設定都寫在該資料夾內。
- 網頁、命令列、錯誤與診斷訊息、程式註解的中文全部為繁體中文（台灣）與台灣用語；命令列語言代碼為 `zh-TW`，`zh-CN` 等其他中文代碼也會顯示繁體中文。

README 截圖使用合成示範資料。

### 未涵蓋

- 未在 Linux 與 macOS 上實機測試。
- 「定價設定」視窗只能編輯 Codex 費率。
