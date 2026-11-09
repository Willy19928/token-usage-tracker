# 第三方授權

本程式直接使用的套件：

- `modernc.org/sqlite`：BSD-3-Clause
- `golang.org/x/sys`：BSD-3-Clause
- `github.com/klauspost/compress`（僅 `zstd` 解壓縮）：BSD-3-Clause

`internal/timezone` 內含 Go 標準函式庫的部分程式碼，授權見同資料夾的 `GO-LICENSE`（BSD-3-Clause）。

間接相依套件的授權隨各自模組保留；在專案資料夾執行 `go list -m all` 可查看實際使用的完整相依清單。

## 上游專案

本專案部分程式碼來自上游專案，以 MIT 授權提供；依其條款保留原授權聲明如下：

```text
MIT License

Copyright (c) 2026 Codex Usage contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
