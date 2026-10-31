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

## 本次本地整理验证

已通过 `bash -n`、路由 `go test -race`、路由 `go vet`、全仓库 `go test ./...`。网关与路由的 Linux amd64 二进制已构建至 `local-45/dist/`，该目录被 Git 忽略。首次验证命令在旧工作目录执行时未找到脚本，改为明确进入本仓库后完整验证成功。部署脚本仅做语法检查，尚未实际执行；本次未再次部署或重启服务器。
