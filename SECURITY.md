# 安全說明

## 設計上的保護

- 服務只監聽 `127.0.0.1`，設定成其他位址時會拒絕啟動。
- 非 GET 的 API 請求必須來自本機來源（Origin 檢查），避免其他網站從瀏覽器觸發掃描或修改設定。
- 網頁使用嚴格的 Content-Security-Policy，不載入任何外部腳本、樣式或圖片，並禁止被嵌入其他頁面。
- 統計用量時不讀取帳號憑證；不保存回覆、思考內容或工具輸出。Claude Code 與 Antigravity 帳本只保存計數、模型、時間與雜湊後的識別碼；Codex 統計另外保存 Codex 自己記錄的對話標題（通常是第一句提示詞）與專案路徑，分享匯出檔前請留意。
- 不上傳用量資料、沒有遙測。對外連線只有自動檢查更新與 Claude Code 額度兩種（後者預設關閉），見下面兩點。
- 自動檢查更新只向 GitHub 查詢最新版本號，可以關閉。只有使用者明確選擇版本後才會下載；更新檔必須與發佈頁 SHA256SUMS 相符，且只接受本專案 GitHub 發佈頁的下載網址。替換前會備份程式、所有帳本與額度紀錄，新版沒有正常啟動時自動還原。
- Claude Code 額度只有在使用者於「額度」頁（或 `config.json` 的 `claude_usage_api`）開啟後才會連線：每 5 分鐘查詢 `https://api.anthropic.com/api/oauth/usage` 與 `https://api.anthropic.com/api/oauth/profile`，登入快到期時向 `https://platform.claude.com/v1/oauth/token` 更新登入。
- Claude Code 的登入檔（`~/.claude/.credentials.json` 或 `$CLAUDE_CONFIG_DIR/.credentials.json`）只在開啟 Claude Code 額度後才會讀取；更新登入時以原子寫入把新權杖寫回原檔，其他欄位保持不變。權杖只送往上述 Anthropic 端點，不寫入日誌，也不另外保存。關閉後不再讀寫這個檔案。
- Codex 的 `auth.json` 只取用身分資訊（電子郵件、帳號 ID、方案）來辨識帳號，不取用也不保存其中的權杖。
- Antigravity 額度只向 `127.0.0.1` 上 Antigravity 自己的語言伺服器查詢；探索檔 `~/.gemini/antigravity/daemon/hub_*.json` 中的本機 CSRF 權杖只用於這些本機請求。
- 讀取 Antigravity 對話資料庫時使用唯讀模式。
- 安裝時會把服務資料夾（三個帳本、額度紀錄與設定）的存取權限縮為只有目前使用者。

## 回報問題

發現安全問題時，請不要直接公開細節，先私下聯絡維護者，並附上：

- 受影響的版本（`token-usage version`）
- 重現步驟
- 可能的影響範圍
