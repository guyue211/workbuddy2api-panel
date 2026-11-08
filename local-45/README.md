# 本地维护 → 合并上游 → 部署到 45

## 当前基线

- 上游： https://github.com/misakano7545/workbuddy2api-panel
- remote：`upstream`（未配置自己的发布远程，不会推上游）。
- 本地分支：`local/45`，起点 `v2026.10.30.1049` / `53735780d850c645d84c15f009324f4cc3153c65`。
- 2026-10-31：已合并上游 `main`（`8ab87d4`，含已发布 tag `v2026.10.31.845` 之后 7 个提交），45 现运行 `2026.10.31.1343`。
- `cmd/server`：上游 Go 网关。
- `cmd/wb-router`：本地跨域路由，包含 2026-10-30 修复的 SSE 额度错误识别、国际优先 → 国内回退和测试。由现有 `../wb-router` 复制；今后以本仓库这一份为维护入口，避免两处修改。
- `local-45/build.sh`：构建 Linux amd64 网关和路由。
- `local-45/deploy.sh`：显式选择部署组件、校验、备份、原子替换、重启、健康检查，失败尝试回滚。
- 配置、API key、账号和运行状态仍留在 45，未下载到仓库。

## 45 上的代理池（/opt/proxypool）

45 自带的代理池服务不是本仓库的一部分，网关通过它的**桥接端口**接入：

- 面板 `127.0.0.1:8080`（Bearer API Key 见 `/opt/proxypool/data/panel.db` 的 `users.api_key`），网关 `0.0.0.0:20000`（HTTP + SOCKS5，需代理池账号密码）。
- `BRIDGE=1`：sing-box 把每个协议节点（ss/vmess/vless/trojan/hysteria2 等）桥成一个本地 SOCKS 入站，监听 `127.0.0.1:30000+`，端口记录在 `nodes.bridge_port`。网关要用的就是这个端口，不需要节点凭据，也不挑协议。
- 池规模：约 5300 个节点、2800 个健康、1900 个已桥接。健康检查由代理池自己按 `HEALTH_INTERVAL=1800` 维护。

接入方式（2026-10-31）：从 `nodes` 里挑 `enabled=1 and healthy=1 and bridge_port>0 and proto='socks5' and quality>=85 and latency_ms<1200` 的节点，按 US/JP/HK/SG/KR/GB/TW/FR 每区取前 3 个，共 19 条，以 `socks5h://127.0.0.1:<bridge_port>` 导入 workbuddy2api 的代理池：

```bash
curl -sS -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  --data '{"lines":"socks5h://127.0.0.1:30347\n..."}' \
  http://127.0.0.1:8788/panel/api/proxies
```

15 个账号各绑一个固定出口（`PUT /panel/api/accounts/{uid}/proxy`，`{"mode":"fixed","proxy_id":"..."}`）：3 个 cn 账号绑 HK 节点（30365/30342/30338），12 个 global 账号分布在 US/JP/SG/KR/GB/TW/FR 节点上。绑定存在 `data/proxy-pool.json` 的 `bindings` 里，realm 默认出口（`realms`）未设置。

注意：

- 固定绑定没有回退：绑定的桥接节点挂了，该账号会直接报 `no healthy proxy available`，需要在面板里换绑或改成随机。
- 桥接端口跟随代理池节点生命周期；节点被删/重排后端口可能消失，届时按上面同一条 SQL 重新挑节点导入并改绑。
- 代理池自己的健康检查是 1800s 一轮，workbuddy2api 对导入的节点是 60s 一轮（默认 `health` 设置），两边口径不同。

## 第一次保存本地改动

当前改动未提交；检查后提交到 `local/45`，这样以后合并上游时能保留路由与部署脚本。不要把 dist、配置或凭据提交进去。

```bash
git status --short
```

## 更新流程

先保存/提交本地改动，保持工作区干净，再拉取上游：

```bash
git fetch upstream --tags
```

查看版本，选择已发布 tag，不盲目追 main：

```bash
git tag --sort=-version:refname
```

以下 `NEW_TAG` 和 `NEW_VERSION` 分别替换成选中的 tag 和不带 v 的版本号：

```bash
git merge NEW_TAG
```

解决冲突后，运行上游测试与路由回归，再构建：

```bash
go test ./...
```

```bash
bash local-45/build.sh NEW_VERSION
```

该构建会注入 `main.appVersion`，避免部署显示 dev；不修改上游版本源码。打包时构建整个路由包，不能单独编译 main.go。

## 部署

默认目标为 SSH 别名 `tokyo112`。脚本要求手动输入 `deploy`，不会只因构建成功就上传。需要已有 SSH 配置、远端 root 权限和可用的 systemd 服务。

只更新网关（上游更新通常用这个，本地路由不动）：

```bash
bash local-45/deploy.sh gateway
```

只更新本地路由：

```bash
bash local-45/deploy.sh router
```

同时更新：

```bash
bash local-45/deploy.sh all
```

脚本不改 config.json/auths/data、不调整账号、不覆盖 systemd unit。暂存与回滚备份留在服务器，便于检查。部署会短暂中断所选服务的在途请求，选空闲时执行。

健康检查只能证明服务可达，不能证明模型调用或跨域回退正常；更新后还需经本机 18788 发一个小请求并检查 `data/router.log` 和 `data/request-logs`。本地 18084 是另一个 opencode2api 的面板，不属于本仓库。

## 2026-10-30 自动回退修复

国际额度耗尽时会返回 HTTP 200 + `event: response.failed`（`error.code=6004` / `insufficient_quota`），旧路由误判成功。新路由在提交下游响应前预检开头事件并回退国内；已收到正文/工具调用后不重试，避免重复执行。正常流字节完整回放。

45 上已有该修复，22:43 实测国际失败 → 国内成功，带前缀和裸模型名均通过。部署备份：`/opt/backups/wb-router-streamfix-20261030T144252Z/`。

## 2026-10-31 合并上游并更新网关

上游 `main` 领先 13 个提交（代理池与按账号路由 #122、窗口用量调度 #112、`/v1/models` 补 models.dev 形状 #119、`web_search_result` 字段补齐 #121、登录设备码头族与 5.9.4 国际版对齐、面板请求上下文改弹窗等），已合并进 `local/45`，合并提交 `683cc5d`，无冲突。

`go build ./...`、`go test ./...` 全绿后按 `bash local-45/build.sh 2026.10.31.1343` 构建，只部署网关（路由源码未变，`wb-router` 未替换）：

```bash
bash local-45/deploy.sh gateway
```

- 部署前版本 `2026.10.30.1049`，部署后 `2026.10.31.1343`，`workbuddy2api-panel` 于 05:45 UTC 重启，`wb-router` 未动。
- 回滚备份：`/opt/backups/wb-local-45-YXWTlnFj/`。
- 验证：两个 `/healthz` 均 healthy=15；经 18788 发非流式与流式各一次 `glm-5.3-flash`，均 200；`data/router.log` 记 `glm-5.3-flash -> global (200 ...)`；`data/request-logs/requests-2026-10-31.jsonl` 持续记录新版本产生的 `/v1/responses` 成功请求。

## 2026-11-04 合并上游（72 个提交，含 tag v2026.11.4.1120）

上游 `main` 从 `1a81f25` 走到 `e6dd088`，合并进 `local/45`（合并提交 `701c484`）。主要变化：

- 出站报文对齐官方真机：头名/顺序、`Connection` 头、TLS 指纹复刻 Node/OpenSSL（JA3+JA4），并有真机夹具对拍进 CI。新增配置 `upstream.tls_fingerprint`，缺省 `node`（不配也复刻），排障可设 `go` 回滚。
- 代理路径：经代理的 HTTPS 先建隧道再走指纹握手（#135）；代理全被停用时随机绑定回落直连（#150）。
- 流式与连接：关闭前读掉流尾、`Close` 幂等，上游连接可复用；上游心跳帧不再吞掉紧随的 Responses 事件（#158）；客户端断开不再记成上游断流。
- 面板：账号分组、事件日志可清空/下拉记忆/20-30-50 条；鉴权改成「默认拒绝」；配置页可分层编辑 cn/global 的 `chat_base`/`billing_base`；默认网关档位可配置；事件日志区分客户端原始档位与网关补档。
- 其他：补 Claude Code 的计数端点与模型别名通配；`model_fallback` 支持域前缀键；6020 带 `retry_after` 不再换号；到期加权不再低于关闭时权重、窗口默认 30 天。

本地冲突两处，均在 `internal/server`：

- `handler.go` 的 `modelAlias`：上游新增通配键（`claude-*`），本地有 `forcedModelAliases` 强制表。合并顺序定为「config 精确键 → config 通配键 → 内置强制表」，强制表仍保留 hidden/allowed 的绕过防护。
- `model_alias_test.go`：两边新增的测试都保留。
- `model_fallback_test.go` 的前缀键用例改用 glm：`global:deepseek-v4.1-flash` 现在被强制表改写成 `-sg`，第一跳就不是原模型，会掩盖前缀键降级的覆盖。

`go build ./...`、`go vet`、`go test ./...` 全绿后按 `bash local-45/build.sh 2026.11.4.1432` 构建。路由源码未变，只需部署网关。

## 2026-11-05 合并上游（9 个提交，含 tag v2026.11.5.1428）

上游 `e6dd088` → `6f19b76`，无冲突，合并提交 `486da9c`。主要变化：

- **出视频端点**（`/v1/videos` 等）+ 任务查询，国际版也能出片、列表不再隐藏 global 视频模型（#162）。
- **MCP 端点**（生图 / 改图 / 出视频 / 查任务）。
- **可选水印裁切**：`features.watermark_strip_enabled`，默认关闭；回读图片不跟随重定向（白名单只校验首跳主机）。
- **tools 脱敏**（借鉴参考仓库）+ 轮询指纹跳过。
- 面板：配置页分域 tab 不再自我隐藏，修四处缩进 /「卡中卡」排版；签到响应回显真实 realm（不再写死 `"cn"`）。

`go build ./...`、`go vet ./...`、`go test ./...` 全绿后按 `bash local-45/build.sh 2026.11.5.1731` 构建。路由源码未变，只需部署网关。

## 2026-11-06 合并上游（16 个提交，含 tag v2026.11.5.2306）

上游 `6f19b76` → `6df19e5`，无冲突，合并提交 `54c7311`。这批全是 11-05 那轮视频/图片/MCP 的修复与规范对齐，未改 config 结构。

- **视频回执改成 OpenAI Video 资源形状**：`status` 归一 `queued/in_progress/completed/failed`，`seconds` 改字符串，`error` 改 `{code,message}`，补 `progress`/`expires_at`/`remixed_from_video_id`；新增 `POST /v1/videos`、`GET /v1/videos/{id}/content`，与原路径共用实现。按旧形状解析的客户端会受影响。
- **图片**：`response_format=b64_json` 现在真回 base64（网关取直链自转，取图失败保留 URL 并记日志）；`n>1` 直接 400（上游只回一张，不再静默少给）。
- **视频任务查询不再扇出整池**（#177）：台账未命中时走有界探测——负缓存 30s + 游标轮转、单次最多 5 个账号、全局 30 次/分钟；429/5xx 立即停手并全局暂停 30s，不罚账号。台账命中但账号已不在池中直接 404。
- **重启后收养的视频任务不重复记账**（#175，实测曾重复计 57.14 积分）；MCP 无 `id` 的请求按通知回 202（#173）；客户端断开只看请求 ctx（#166）；裁水印先读文件头查像素上限（#168）；清空归档的请求通道改无缓冲（#170）。

`go build ./...`、`go vet ./...`、`go test ./...` 全绿后按 `bash local-45/build.sh 2026.11.6.032` 构建。路由源码未变，只需部署网关。

## 45 上的模型上限手编覆盖（data/model.json）

上游目录对 deepseek 系列报 128k 输出，实际 384k。`model.json` 里 `"source": "manual"`
的条目是查找链第 0 级，压过上游动态值，且 models.dev 回填不会覆盖它。45 上现有三条：

```json
"deepseek-v4.1-flash":    {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"},
"deepseek-v4.1-flash-sg": {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"},
"deepseek-v4-pro":        {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"}
```

`-sg` 是 global 域 `deepseek-v4.1-flash` 的付费变体（`forcedModelAliases` 把请求改写过去），
上游单独给它报了 128k，所以也要手编。改完 `systemctl restart workbuddy2api-panel` 生效；
部署脚本不碰 `data/`，手编条目会跨版本保留。

## 本次本地整理验证

已通过 `bash -n`、路由 `go test -race`、路由 `go vet`、全仓库 `go test ./...`。网关与路由的 Linux amd64 二进制已构建至 `local-45/dist/`，该目录被 Git 忽略。首次验证命令在旧工作目录执行时未找到脚本，改为明确进入本仓库后完整验证成功。部署脚本仅做语法检查，尚未实际执行；本次未再次部署或重启服务器。

## 2026-11-08 合并上游（43 个提交，含 tag v2026.11.8.1203）

上游 `6df19e5` → `4b0a3b4`（tag `v2026.11.8.1203` 打在 `29db878` 上），合并进 `local/45`，合并提交 `a51c6b2`，**无冲突**。`git merge-tree` 预演与实合并都干净，`cmd/wb-router` 在这 43 个提交里一行未动。

主要变化：

- **输出预算与续写**（`2b9b773`/`562d0d8`/`34a3ae6`/`e54c6e0`/`29db878`）：高档位思考把额度吃光时按档位抬输出预算下限；抬过额度的请求在 `finish=length` 处自动续接；续写闸门从「最多 3 段」改成「累计输出上限」，低档位长文不再停在半句。新增 `internal/upstream/output_budget.go` 与 `continue_*_test.go`。
- **默认档位改 high**（`ffca1da`）：`EffortPolicy.Normalized()` 缺省值 `max` → `high`；`/v1/responses` 只有 reasoning item、正文与工具调用都为空时改判失败（流式 `response.failed` code `empty_reasoning_only`、非流式 502），不再回 `response.completed` 骗客户端收尾。
- **reqlog / 面板全链路区分 `model_eff`**（`ee5ae70`/`efe64fd`/`b674f71`）：降级前后模型分开记，按线协议原值、不自造域前缀。
- **稳定性**：上游断流补截断终态（`84a48d9`）、`11134` 供应商过载不罚号不换号并保留会话粘性（`4830686`）、入站请求体上限与 `11140` 风控连坐保护（`b494426`）。
- **认证**：支持新版桌面端的 `$wbEncrypted` auth 文件（`c7cc216`），回写只封「读入时确实加密」的字段（`97d3f25`）。
- **性能**：请求体只解析一次、指纹预检去掉 RE2 大小写折叠（`4b0a3b4`/`fe51fd6`），`WB2API_PPROF` 诊断开关（`b42712c`）。

`go build ./...`、`go vet ./...`、`go test ./...` 全绿后按 `bash local-45/build.sh 2026.11.8.1456` 构建。路由源码未变，只部署网关：

```bash
bash local-45/deploy.sh gateway
```

- 部署前 `2026.11.6.032` → 部署后 `2026.11.8.1456`，`workbuddy2api-panel` 于 06:57:09 UTC（14:57 CST）重启；`wb-router` 未动（已连续运行 8 天 16 小时）。
- 部署后 `wb2api` 的 sha256 `7cdd3cff…f746ab` 与本地 `local-45/dist/wb2api` 一致。
- 回滚备份：`/opt/backups/wb-local-45-0AMJl3NC`。
- 验证：两个 `/healthz` 均 `healthy=17/17`；经 8788 发非流式与流式各一次 `glm-5.3-flash`，均 200；`data/router.log` 记 `glm-5.3-flash -> global (200 ...)`；重启后 ERR/WARN 计数 0。

### 45 上 default_effort 实际仍是 max

`ffca1da` 只改了 `internal/upstream/effort_policy.go` 的 `Normalized()` 与 `config.example.json`；`cmd/server/config.go:563` 和 `normalizeThinking()`（`config.go:1236`）仍把空值归一成 `max`。45 的 `config.json` 没写 `thinking.default_effort`，配置层先填 `max`，`Normalized()` 看到的是非空值，不会改 high。

部署后实测：`/panel/api/models` 的 `gateway_effort_wanted` = `max`；请求记录 `reasoning_effort=max`、`reasoning_effort_requested` 为空；流水行仍是 `effort=- ->max`，与部署前一致。

要真正生效，在 45 的 `config.json` 的 `thinking` 节显式写 `"default_effort": "high"`（或面板「模型与档位」页逐模型写 `thinking.model_defaults`），保存即热生效，不必重启。

### wb-router 未替换

45 上跑的仍是 `~/work/ai/wb-router` 那份 10.5 MB 构建（`4fe27e01…`），其 `main.go`/`stream.go`/`stream_test.go` 与本仓库 `cmd/wb-router/` 逐字节相同，行为一致；差别只在构建来源（独立模块 `go 1.21`、未 strip、无 vcs 戳）。要让它变成仓库产物，跑 `bash local-45/deploy.sh router`，代价是路由重启、在途请求中断一次。

## 2026-11-08 截断复查：客户端限额被当成「它只要这么短」

「经常半路截断」的复查结论与修复。**根因不在上游，在续写闸门**：客户端只要带了
`max_tokens`，`continue.go` 就按「客户端有意只要这么多」直接收尾，不再续接。

### 实测（影子实例直连，global:deepseek-v4.1-flash-sg，同一句「数到 20000」）

| 请求 | completion | finish | 结果 |
| --- | --- | --- | --- |
| `max_tokens=32000` | 32000 整 | length | 正文 47853 字符停在 `9792,` 之后，**半句，无续写** |
| `max_tokens=40000` | 40000 整 | length | 同上（半句） |
| 不带额度（网关按目录补 384000） | 60660 | stop | 数到 12000 后自然收尾 |
| 修复后 `max_tokens=32000` | 36594（两段） | stop | 数到 12000 + `DONE`，**完整** |

结论两条：

1. **上游没有 32000 的硬上限**（`continue.go` 开头那条是 CN 域旧实测，global 域不复现）。
   上游是「客户端给多少就在多少处切」——`max_tokens=40000` 就切在 40000。
2. 所以「客户端限额」在 32000 以上这一档不代表「只要这么短」，只代表「先给个上限」。
   按它收尾等于把上限当成意愿，用户看到的就是半路截断。目录 `/v1/models` 对外报
   `max_output_tokens=384000`（手编覆盖），照着它填 `max_tokens` 的客户端全都落进这一档。

### 修复（`internal/server/continue.go`）

- `continueClientLongAnswer = 32000`：客户端额度 ≥ 它时按「要长文」处理，`finish=length`
  继续续写，直到正文 token 写够客户端额度（思考不占这份预算，见 `totalText`）。
  额度小于它的仍按客户端意图收尾（8192/4096 多半是「就要一小段」）。
- `outputCapped` 增加第二道闸：`totalText() >= clientLimit` 时停止续写（不会无限续）。
- `continueCapped` 只在 `finish=length` 时置位——正常收尾（`finish=stop`）不再被日志
  写成「按截断收尾」（影子实测踩到：客户端看到 stop，日志说截断）。

新增/改写测试：`TestContinueAllowedWhenClientLimitBeyondSegmentCap`、
`TestContinueStillBlockedForSmallClientLimit`、`TestOutputCappedByClientTextBudget`、
`TestContinueCappedNotFlaggedOnNormalStop`。`go build ./...`、`go vet ./...`、
`go test ./...` 全绿。

### 未做

- 工具参数截断（半个 JSON）仍不续（`continue.go` 的 ponytail 注记）：拼接残缺
  `tool_calls` 比不续更糟。agent 客户端在这一形态下仍会看到「回合结束但没有工具调用」。
- 部署：本次只验证到影子实例，未动 45 上的二进制。要上线按
  `bash local-45/build.sh <版本号>` 后 `bash local-45/deploy.sh gateway`。

## 2026-11-08 续：默认档位一致性 + 工具参数截断取证

### 默认档位：两处写死的缺省值打架

`thinking.default_effort` 的缺省值此前有两个版本：

- `internal/upstream/effort_policy.go` 的 `Normalized()`：空 → `high`（附实测：同一
  8192 额度下 high 的正文比 max 多 70%）；
- `cmd/server/config.go` 的 `Default()` 与 `normalizeThinking()`：空 → `max`。

config 层先填值，policy 层的 `high` 永远看不到，于是**上游 ffca1da 想要的 high 从来没
生效过**，45 上一直是 `effort=- ->max`。

改法：新增 `upstream.DefaultGatewayEffort = "high"` 作为唯一事实来源，两处都引用它；
`TestThinkingDefaults` 改成断言 high。

**45 的行为没变**：部署前在 `config.json` 显式写了 `"default_effort": "max"`
（备份 `/opt/backups/config.json.before-effort-pin-*`），部署后首条流水仍是
`effort=- ->max`。要切 high 就改这一行，保存热生效：

```bash
ssh tokyo112 'python3 - <<EOF
import json; p="/opt/services/workbuddy2api-panel/config.json"
c=json.load(open(p)); c["thinking"]["default_effort"]="high"
json.dump(c,open(p,"w"),ensure_ascii=False,indent=2)
EOF'
```

理由：max 思考更深、单轮更贵；high 正文更多、更省。45 是自用 agent 池，保持现状由
使用者决定，不在代码修复里顺手改掉。

### 工具参数截断（V2「剩余参数改写」）：取证后不做

两次探针（`probe_tool.py`，声明 `write_file` 工具）：

| 请求 | completion | finish | 工具参数 |
| --- | --- | --- | --- |
| `max_tokens=600`，写 8000 字 | 7307 | tool_calls | 11608 字节，可解析 |
| `max_tokens=32000`，写 60000 字 | 21653 | tool_calls | 35223 字节，可解析 |

`max_tokens=600` 那条没被切，是因为网关的输出预算下限先把额度抬到 8192
（`output_budget.go`）；两条都是模型自己收尾，没有撞上限。线上日志里
`[continue] 跳过：本段含工具调用分片` 至今 **0 条**。

结论：**V2 不做**。判据写在这里——等这条日志真的开始出现（说明有客户端稳定撞上
参数截断），再做「残缺 arguments 作前缀 + 只接受连续片段」的续写。现在做等于给一个
没发生过的形态写拼接逻辑，风险（模型重发开头、提前补 `}`）大于收益。
