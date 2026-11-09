# 計量與計價說明

這份文件說明 Token Usage 如何從三個來源取得用量、如何去重與歸屬，以及費用怎麼算。

## 共通原則

- **只增不刪**：已記錄的用量不會因為原始檔案被刪除而消失。重新掃描只會新增或修正，不會移除。
- **修正留痕**：同一筆請求的計數被來源修正時，Codex 會更新原事件；Claude Code 與 Antigravity 會把舊值寫進修訂紀錄表，再更新帳目。
- **只存計數**：Claude Code 與 Antigravity 帳本只保存模型、時間、Token 分類、雜湊後的對話與來源識別，以及來源行號。Codex 統計另外保存 Codex 自己記錄的對話標題與專案路徑。三者都不保存回覆、思考內容或工具輸出。
- **不猜測**：缺少穩定身份、格式不符或計數互相矛盾的紀錄不匯入，並在頁面上提示。
- **時間區間**：自訂區間包含開始時刻、不包含結束時刻，以頁尾顯示的計量時區解釋。
- **Token 分類**：在網頁與匯出中，Cached Input 與 Cache Write 是 Input 的子集，Reasoning 是 Output 的子集，Total 不會重複加總。

## Codex

### 來源

讀取 `CODEX_HOME` 下 `sessions/` 與 `archived_sessions/` 的 JSONL。Codex 狀態資料庫只用來找出檔案路徑並補充標題、專案等資訊，資料庫裡的 `tokens_used` 不計入總量。

Codex 會把較舊的對話壓縮成同名的 `.jsonl.zst` 並刪除原檔。程式把壓縮檔當成原本那個 JSONL，從上次讀到的位置接著讀解壓後的內容，所以服務停止期間才寫入、隨後被壓縮的用量也會補記，已記錄的部分不會重複計算。同名的 `.jsonl` 仍在時以 `.jsonl` 為準。

### 去重與歸屬

- 新格式紀錄以 `token_usage_record` 為準，依對話與 `response_id` 去重，並用 `turn_token_usage` 核對已入帳的增量；一般回應與壓縮請求都會計入。
- 沒有獨立請求紀錄的舊格式以累計值差分計算；重送舊快照或從中途恢復時只計新增部分。
- 一個 JSONL 的所屬對話由第一筆 `session_meta` 決定。Fork 檔案中複製進來的父對話歷史只作為基準，不算成子對話的新用量。
- 用量歸到紀錄時間所在的計量日，不依對話最後更新時間歸日。
- 檔案被截斷或改寫時保留現有統計並提示；只有在網頁上確認，或執行 `token-usage scan --rebuild`，才會從現存 JSONL 重建。已刪除的 JSONL 在重建後無法恢復。只有修改時間變動、大小與內容不變的檔案（例如被備份或防毒軟體動過時間）會先核對開頭雜湊與最後的累計 Token，相符就繼續掃描，不會提示重建。

### 計量時區

計量時區以 IANA 名稱存在統計庫中，所有程序與瀏覽器使用同一個時區。第一次建立統計庫時採用 `TOKEN_USAGE_TIMEZONE`，沒設定則使用系統時區；之後更改系統時區不會影響已保存的值。

### 計價

- 使用內建的 OpenAI Standard 文字價格表（USD／每 100 萬 Token），Cached Input 與 Cache Write 從 Input 扣除後分別計價，Reasoning 已包含在 Output 中。
- 明確標記為 Fast 的用量，費用按 Codex 額度倍率折算：GPT-6.1 Sol、GPT-6 Astra／Sol／Luna、GPT-5.6 系列與 GPT-5.5 為 2.5 倍，GPT-5.4 為 2 倍。沒有已確認倍率的模型會標為未定價。Token 數本身不乘倍率。
- `codex-auto-review` 預設以零元計算，Token 照常統計；可以在「定價設定」覆寫。
- 可在「定價設定」把內部模型指定為某個內建模型的別名，或填入自訂單價。設定存在 `config.json` 的 `pricing_overrides`：

```json
{
  "pricing_overrides": {
    "internal-review": { "alias_of": "gpt-6.1-sol" },
    "internal-model": {
      "input_usd_per_million": "1.00",
      "cached_input_usd_per_million": "0.10",
      "cache_write_input_usd_per_million": "1.25",
      "output_usd_per_million": "6.00"
    }
  }
}
```

## Claude Code

### 來源

遞迴讀取 `~/.claude/projects` 與 `~/.config/claude/projects` 下的 JSONL，包含子代理與已同步到本機的 SSH 會話。`CLAUDE_CONFIG_DIR` 可改為其他資料夾（逗號分隔）。檔案最後一行若尚未寫完，留到下次掃描。

### 去重與歸屬

- 每個請求以「message ID＋request ID」識別；沒有 request ID 時改用「對話＋message ID」。同一請求的多個內容區塊、跨檔複製只計一次；同一請求出現多個版本時取各分類都不小於前者的完整值。
- 子代理（sidechain）檔案中重播的父對話訊息不重複計算；已存在的這類舊紀錄會標記排除並保留稽核紀錄。
- `advisor_message` 迭代以自己的模型另外記一筆，一般迭代不重複加總。
- 模型以當次選擇為準（有 `requestedModel` 時），版本寫法統一，例如 `claude-opus-5.5` 與 `claude-opus-5-5` 視為同一模型。

### Token 分類

- `output_tokens` 已包含思考 Token；有拆分資料時顯示為 Reasoning，計價只算一次。
- 快取寫入分成 5 分鐘與 1 小時兩種，分別計價；不把快取寫入當成一般輸入。

### 計價

依 Anthropic 官方 Standard API 日期費率（USD／每 100 萬 Token），從各模型生效日起適用。「推出時起」表示該模型的價格自推出後沒有變動：

| 模型 | 生效日 | Input | Output | 快取讀取 | 5 分鐘寫入 | 1 小時寫入 |
|---|---|---:|---:|---:|---:|---:|
| claude-fable-5-2、claude-mythos-5-2 | 推出時起 | 8 | 40 | 0.20 | 10 | 16 |
| claude-fable-5-1、claude-mythos-5-1 | 推出時起 | 10 | 50 | 0.25 | 12.50 | 20 |
| claude-fable-5、claude-mythos-5 | 推出時起 | 10 | 50 | 1 | 12.50 | 20 |
| claude-opus-5 | 推出時起 | 5 | 25 | 0.50 | 6.25 | 10 |
| claude-opus-4-8 | 推出時起 | 5 | 25 | 0.50 | 6.25 | 10 |
| claude-opus-4-7 | 推出時起 | 5 | 25 | 0.50 | 6.25 | 10 |
| claude-opus-4-6 | 2026-02-05 | 5 | 25 | 0.50 | 6.25 | 10 |
| claude-sonnet-5 | 推出時起 | 2 | 10 | 0.20 | 2.50 | 4 |
| claude-sonnet-4-6 | 推出時起 | 3 | 15 | 0.30 | 3.75 | 6 |
| claude-haiku-4-5 | 推出時起 | 1 | 5 | 0.10 | 1.25 | 2 |
| claude-opus-5-5 | 2026-09-22 | 4 | 20 | 0.20 | 5 | 8 |
| claude-sonnet-5-5 | 2026-10-07 | 2 | 10 | 0.10 | 2.50 | 4 |
| claude-haiku-5-5（單次輸入 ≤ 100,000） | 2026-10-07 | 0.10 | 0.50 | 0.01 | 0.125 | 0.20 |
| claude-haiku-5-5（單次輸入 > 100,000） | 2026-10-07 | 0.50 | 2.50 | 0.05 | 0.625 | 1 |

- 快速模式為 2 倍（Fable 5.2、Mythos 5.2、Opus 5.5、Opus 5、Opus 4.8），美國推論地區為 1.1 倍。
- Claude Code 在本機產生、未呼叫 API 的訊息（模型標示為 `<synthetic>`）不列入統計。
- 可在「定價設定」的 Claude Code 頁填入自訂單價（含 1 小時快取寫入），並指定生效日、結束日（不含）與提示門檻；手動單價優先於內建費率，不再套用快速模式與地區倍率。設定存在 Claude 帳本的 `pricing-overrides`。
- 生效日之前的用量、未知速度模式、未知推論地區，或缺少快取時效的紀錄，都標為未定價，不拿現在的價格回推。
- 不含網頁搜尋、程式執行等工具附加費。

## Antigravity

### 來源

掃描 `~/.gemini/antigravity`、`antigravity-ide`、`antigravity-cli` 三個資料夾的 `brain/<對話>/.system_generated/logs/transcript.jsonl`，並唯讀開啟對應的 `conversations/<對話>.db`。`ANTIGRAVITY_TRACKER_HOME` 可改為其他資料夾。

### 用量來源的優先順序

每一筆規劃回應（`PLANNER_RESPONSE`）依序取用：

1. **對話資料庫 `gen_metadata`**：原生用量統計，含輸入、快取讀取、快取寫入、總輸出、思考與回應輸出。
2. **對話資料庫 `steps` 中的用量**：`gen_metadata` 沒有該步驟時使用。
3. **transcript 內的計數欄位**：資料庫都沒有時使用。
4. **文字估算**：以上皆無時，以字元數估算（中日文每字 1 Token，其他每 4 字元 1 Token），並標為「估算」，與實際計數分開顯示。

已經存有實際計數的紀錄，不會被估算值或較粗略的 transcript 計數覆蓋。

### 模型歸屬

以當次的模型選擇為準：優先採用資料庫記錄的選擇標籤，其次是 transcript 中「Model Selection」設定變更的時間軸。名稱統一成小寫並去掉思考等級，例如「Gemini 3.1 Pro (High)」記為 `gemini-3.1-pro`。

### 計價

依各模型的日期費率（USD／每 100 萬 Token）：

| 模型 | 生效日 | Input | Output | 快取讀取 | 快取寫入 |
|---|---|---:|---:|---:|---:|
| gemini-3.1-pro | 2026-02-19 | 2 | 12 | 0.20 | 未確認 |
| gemini-3.1-pro（單次輸入含快取 > 200,000） | 2026-02-19 | 4 | 18 | 0.40 | 未確認 |
| gemini-4-argon | 2026-10-13 | 2 | 10 | 0.10 | 未確認 |
| gemini-4-argon | 2026-12-01 | 4 | 20 | 0.20 | 未確認 |
| gemini-3.8-flash | 2026-09-02 | 0.75 | 3.75 | 0.075 | 未確認 |
| gemini-3.8-flash | 2027-01-01 | 1.50 | 7.50 | 0.15 | 未確認 |
| claude-opus-4-6 | 2026-02-05 | 5 | 25 | 0.50 | 6.25 |
| claude-opus-5-5 | 2026-09-22 | 4 | 20 | 0.20 | 5 |
| claude-sonnet-5-5 | 2026-09-28 | 2 | 10 | 0.10 | 2.50 |
| claude-sonnet-4-6 | 推出時起 | 3 | 15 | 0.30 | 3.75 |

- 有快取寫入、但該模型的快取寫入費率未確認時，該筆標為未定價。
- 路由模型 `gemini-pro-default` 只有在當次明確選擇「Gemini 3.1 Pro」時才計價，否則列為暫不計價。
- 可在「定價設定」的 Antigravity 頁填入自訂單價，並指定生效日、結束日（不含）與提示門檻；手動單價優先於內建費率。設定存在 Antigravity 帳本的 `pricing-overrides`。

## 「整體」如何合併

- Token、事件數、對話數直接相加；每日與每小時趨勢依日期與小時合併。
- 模型分組以統一後的模型名稱合併；來源分組會出現 Codex 的各來源，以及「Claude Code」、「Antigravity」。
- 費用由各來源依自己的規則先算好再相加，不會用 Codex 價格表去算 Claude Code 或 Antigravity 的用量。
- 對話列表依最後使用時間排序，三個來源混合顯示。

## 額度頁的 Token 合計

「額度」頁的已用百分比與重置時間直接採用各工具回報的數值，不由 Token 推算（來源見 README 的「額度」）。每一期旁邊的 Token 合計，是該工具從「重置時間往前推一個週期長度」起、到重置時間（尚未重置時為現在）為止的全部用量，依本文件同樣的去重規則計算，不依帳號拆分；同一段時間用過多個帳號時，合計會包含所有帳號。
