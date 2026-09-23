# 更新日志

## 2026-09-23 — 治理与减重轮

一轮以“行为不变、现有测试即合同”为前提的治理：写入门禁收敛到 `/api/editor/v1/*`，离线工具与 Sekaipedia 配置数据移出生产二进制，四个超大文件按职责拆开。本轮新增数据库迁移 v35 与 v36。

### 破坏性变更

- 删除 9 条无版本号写路由（`PUT /api/entry`、`PUT /api/category/batch`、`PUT /api/lyrics/save`、`POST /api/lyrics/translation-editions`、`POST /api/lyrics/publish`、`POST /api/lyrics/unpublish`、`PUT /api/event-story/update`、`POST /api/event-story/promote-human`、`POST /api/backup/push`）→ JSON 404。写入统一走 `/api/editor/v1/*`。保留 `POST /api/admin/lyrics-source-reviews/import`（宽松门禁，SekaiText 仍在用）并新增严格双胞 `POST /api/editor/v1/admin/lyrics-source-reviews/import`。
- 删除 WebSocket hub 与 `/ws`；`gate.status` 改经 SSE 发出，连接即推当前门禁状态。
- 只读接口与探针对写方法返回 405 + `Allow: GET, HEAD`。
- 数据库 schema v35：`song_lyrics_public_withdrawals`。

### 公开歌词投影

- 取消发布会真正从公开文件撤下 bundle 内的歌曲（撤下标记）；发布清除标记；bundle 加载失败时仅用数据库降级重建并上报 Degraded；译本元数据变更与曲名批量修改立即触发全量发布。
- 目录接口 `runtimeLyrics` 改为反映实际公开投影（bundle 覆盖 + 数据库发布 − 撤下），控制台“公开镜像”随投影 generation 刷新。
- 制品 pin 单一来源 `contracts/public-lyrics/baseline.json`；CI 校验已提交归档。

### 控制台

- 歌词编辑器 dirty 判定改用键序无关的 canonical JSON；修复 checkpoint 后无法发布的死循环。
- 官方 CN 条目提示“下次同步会覆盖，长期保留请锁定保存”。
- revision-0 手动歌词可输入日文与注音；ruby 无读音不再被客户端拒收。
- 活动剧情「整篇标记人工」等严格 producer 操作不再在客户端被 409 拦截：producer proof 改为动作结束后再清除（`guardProducerMutation`），写栅栏仍在期间阻挡其他写入。

### P0 止血（14 个提交）

- 服务端 11 项：upstream 同步版本记录、translator 事件重试与占位、682 迁移幂等、恢复备份覆盖度、v3 源层编辑拒绝、collab checkpoint envelope、令牌撤销关房、事件时间戳、config 旧值归一化、restore 触发器/投影缓存、S3 latest 指针。
- 控制台 2 项见上节（revision-0 手动歌词、ruby 无读音）；官方 CN 优先的文档说明（`beda9b3`）见「来源优先级真相」。

### 结构

- 拆出共享的 `lyricscontract` 之后，`offlineimport`、`lyricssourceoffline`、`lyricsproviderpolicy` 等离线包移出生产依赖闭包；`go list -deps .` 已不含任何 `moesekai/server/offline/` 包（端点表由 `lyricssource` 自持，并由 offline 侧的 `endpoint_pin_test.go` pin 到策略表）；新增 `TestProductionBinaryDoesNotLinkOfflinePackages` 守门。
- store 的 recovery-import 测试 fixture 不再依赖离线包（a322647）。
- lyricsctl 删除手抄 flag 表，参数原样转发委托二进制；修复 `-expected-plan-sha256` / `-*-authorization` / catalog-filter `-output` 被 lyricsctl 误拒（ce4b0d0）。
- 数据库 schema v36：`lyrics_provider_page_targets` / `lyrics_provider_contributor_aliases`，种子为原先编译进 `api/server.go` 的 37 条 Sekaipedia 乐曲→页面标题映射与 26 条词曲作者别名；新增管理 API `GET /api/admin/lyrics-providers/sekaipedia/targets`、`PUT`/`DELETE /api/admin/lyrics-providers/sekaipedia/targets/{musicId}`（整表校验后热替换来源注册表，无需重启）与“管理设置 → 歌词来源映射（Sekaipedia）”面板；两表属配置数据，不进入 Git/S3 内容备份。
- 全部离线 operator 命令与 16 个专用包迁入嵌套 Go module `server/offline/`（`moesekai/server/offline`，`replace moesekai/server => ../`）；主模块 `cmd/` 只剩 `migrate`；`production_deps_test.go` 改为断言无 `moesekai/server/offline/` 前缀依赖；端点 pin 测试迁至 offline 侧经 `RecoveryProviderConfig(...).APIEndpoint` 比对；CI 分别在 `server/` 与 `server/offline/` 跑 test/vet/race；主模块 `go mod tidy` 去掉无人引用的 `golang.org/x/net`。删除一次性发行命令 `lyrics-release-today` 与 `lyrics-recovery-acceptance-launcher`（可在 f0039fb 的 `server/cmd/` 找回）。恢复计划的精确源码闭包策略升级为 `moesekai-recovery-source-selection-v3`（六个包根、两份 go.mod/go.sum、允许且仅允许 `replace moesekai/server => ../`）。
- `Console.tsx` 2123 → 487 行：实时与条目逻辑切入 `web/src/components/console/` 的 `useConsoleRealtime`（SSE 事件路由、producer proof、冲突冻结）、`useConsoleEntries`（侧栏加载、选择、章节导航）、`useEntryEditor`，界面切成 `TranslationEntryWorkspace`、`ProducerOperationsShell`（含 `useProducerOperations`）、`ConsoleSidebar`、`EntryRow`、`EventStoryToolbar`，草稿与偏好落到 `console-drafts.ts`、`preferences.ts`、`types.ts`、`useAppUpdateProbe.ts`。
- `LyricsEditor.tsx` 2143 → 251 行：切入 `web/src/components/lyrics/` 的 `lyricsEditorState.ts`（`useLyricsEditorState` / `useLyricsActiveTarget`）、`useLyricsDocumentLoader`、`useLyricsDocumentCommands` 与纯变换 `lyricsDocumentCommands.ts`、`lyricsDocumentModel.ts`、`useLyricsPersistence`、`useLyricsSourceWorkflow`、`LyricsDocumentView`；主组件只做组合与模态框。
- store 大函数分段：`validateRestoredLyricsRecoveryProvenance` 拆为 `recoveryBackupGraph` 的 batch/item/source/artifact/evidence/contribution 校验方法；`importOrderedTx` 拆为 `preservedEventStoryLocalizationsTx`、`deleteReplacedEventStoryRowsTx`、`insertEventStoryMetaTx`、`insertEventStoryEpisodesTx`、`restorePreservedEventStoryLocalizationsTx`、`reconcileImportedEventScenariosTx`；`saveLyricsRenditionMutation` 拆为 load/validate/diff/persist 四步；内容备份导出与恢复导入按表分组为 `*BackupExportQueries` 与 `import*RowsTx`。
- 服务端装配：`NewServer` 移入 `server/internal/api/server_wiring.go`，路由注册留在 `routes.go`，JSON 解码与导入授权分到 `json_body.go`、`lyrics_import_grants.go`；`main.go` 1098 → 98 行，启动装配拆到 `startup.go`、`wiring.go`、`http.go`、`seed.go`、`shutdown.go`、`env.go`、`operational.go`。
- 文档：README 压到四节（这是什么／怎么跑／来源优先级真相／发布怎么工作）；`PRODUCTION_CONTRACT.md` 只保留主站与 operator 依赖面并把迁移清单补到 v36；`LEGACY_BASELINE.md` 把官方 CN 优先从“冻结怪癖”改写为有意行为；`STANDALONE_RELEASE.md` 的制品 pin 统一指向 `contracts/public-lyrics/baseline.json`。

### 验证

- `go vet ./...` 与 `go test ./...` 全绿；web `node --test tests/*.test.mjs` 219 项通过，`tsc --noEmit` 与 `eslint src --max-warnings=0` 零输出；`./scripts/verify-release.sh` 通过。
- Orca 浏览器端到端：登录、v1 保存、活动剧情、歌词保存/发布/撤下、SSE `gate.status`、管理面板。

## 2026-09-19 — 代码审查修复轮

一轮以“不丢失现有功能与行为”为前提的审查，共 19 个提交，全部为缺陷修复与一处文档更正，没有新增功能。数据库 schema 未变更。

### 破坏性变更

- **令牌改为绑定 `users.id`（新增 `uid` 声明），不再仅凭用户名解析用户行。** 升级后所有已签发的令牌都会被拒绝，**全部控制台会话必须重新登录**。控制台收到 401 会清除会话并重新加载，但歌词编辑器没有本地草稿持久化，建议选择无人编辑的时间窗口部署。

  该变更修复的是一个可复现的账号问题：删除用户后以相同用户名和相同角色重建，被删账号未过期的令牌会重新生效，甚至能刷新换取新令牌。新建账号的 `token_version` 从 1 开始，恰好等于未刷新过的登录令牌所携带的值。`users.id` 为 `INTEGER PRIMARY KEY AUTOINCREMENT`，SQLite 不会复用，因此无需变更 schema。

### 安全与信息泄露

- 歌词来源请求失败时不再通过响应头返回上游错误原文（其中包含上游地址、查询串与解析出的地址），改为记录到服务端日志。
- 省略 locale 的旧版分支不再把存储层原始错误作为面向客户端的消息返回，改为返回稳定的错误码。
- 备份加密密钥格式非法时立即失败，不再继续执行。

### 可用性与正确性

- WebSocket：修复向已关闭通道发送导致的崩溃，关机时关闭 hub；连接的关闭动作移出 hub 互斥锁之外；令牌代次变更（改密码、改角色、删除账号、刷新令牌）时同步撤销 WebSocket 流，此前该撤销仅对 SSE 生效。
- 排水期返回的 503 对 `/files` 与 `/translation` 保留允许跨域头，浏览器不再收到无法解释的失败。
- `/files` 的错误响应保留跨域头，并修复增量发布丢失的问题。
- 文件服务对本地化所拥有的条目返回其本地化字节。
- 搜索索引先发布校验通过的索引再持久化缓存；重试退避得以真正执行，不再被防抖挤掉。
- 上游被限流时翻译器会重试。
- 活动剧情推广若未匹配到任何剧情会如实报告。

### 数据完整性

- 歌词编辑保存会重建其自身流程中丢弃的不可变性触发器，此前首次保存后这两张表将永久开放任意 UPDATE。
- 只读导出不再向正在被导出的数据库写入证据链接行。
- 公开歌词包构建脚本锁定到实际发布的数量。
- 恢复流程改用 v27 的删除保护，而非 v25 的。
- 更正文档：内容恢复是替换 source-v3 文档，而不是拒绝。

### 验证

- 门禁：`go test ./...` 55 个包全部通过，`go vet` 通过，`scripts/verify-release.sh` 20/20，`release-workflow` 20/20，提交内容秘密扫描通过。
- 控制台按 CI 口径验证：`npm ci` / `npm test` / `npm run typecheck` / `npm run lint` / `npm run build` 全部通过，静态导出正常。
- 端到端实跑：以修复分支与变更前基线各启一个实例、各用独立数据库做 A/B 对比，逐项确认差异——`/files` 错误响应的跨域头、删号重建后的令牌失效、调试响应头的消失、歌词保存后触发器留存、只读导出零写入。
- 关机对比：在保持 3 条 SSE 与 4 条 WebSocket 连接时发送 SIGTERM，基线超出 25 秒预算并在未关闭 SQLite 的情况下退出，修复后 7 条连接在 10.5 秒内关闭且无 CRITICAL 日志。
- 部署组合实跑：单个 Go 进程同时服务控制台静态导出与 `/api`、`/files`、`/translation`，登录与公开文件分发均正常。
- 未能在线实测的一项：排水期 503 的跨域头。常规 SIGTERM 下监听器几乎立即关闭，该窗口无法稳定命中，改以测试验证（在基线上断言失败，在修复后通过）。
