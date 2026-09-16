# AOK Web Application ABI v1

Web 能力按成本和状态从低到高升级。Agent 申请结构化 capability，runtime 选择满足请求的
最低层；升级不能扩大 URL、身份或数据外发范围。

```text
web.fetch       HTTP/TLS、cookie jar、cache、JSON/RSS/GraphQL
web.document    HTML parser、DOM、selector、table、text、link extraction
web.session     JS runtime、登录态、表单、WebSocket/SSE
web.browser     headless browser、DOM/accessibility tree、network events、screenshot
computer-use   仅对没有结构化接口的外部软件作 fallback
```

## Capability

```json
{
  "kind":"web.session",
  "url_prefixes":["https://example.com/"],
  "methods":["GET","POST"],
  "identity":"browser-profile-7",
  "allow_upload":false,
  "max_response_bytes":10485760,
  "expires_at":1770000000
}
```

URL 前缀、HTTP method、redirect 目标、cookie profile、上传目录和响应大小都是 capability
范围。cookie、authorization header、session storage 和下载文件必须以 handle 或 artifact
引用返回，不得直接拼入 prompt 或普通日志。

## 调用与证据

每次调用生成 `web_request`、可选 `document_snapshot`、`network_trace` 和 `artifact`。
结果包含内容 hash、来源 URL、时间、response status、解析器版本和 taint 标签。DOM、
accessibility tree 与 screenshot 是不同的输出，不得假定 screenshot 可重建 DOM。

`web.fetch` 适合 API、静态页面和协议审计；命令行模拟或抓包不能代替 `web.session`/
`web.browser` 的 JavaScript、复杂 OAuth、WebSocket/SSE、Canvas、上传、反爬挑战和无障碍
树语义。ComputerUse 只在结构化层失败或目标软件没有协议接口时启用，并且仍受同一
network、artifact、taint 和审计策略约束。

## 状态与恢复

session 的 cookie/profile、DOM checkpoint 和 network trace 放入 LSFS，带 TTL、owner
Application 和撤销状态。恢复时报告 `session_exact`、`session_replay` 或 `relogin_required`；
失去登录态不得伪装成成功恢复。外部页面内容默认标记 `external-content` taint，未经
策略允许不得写入长期记忆或外发。
