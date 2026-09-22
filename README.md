# NextTrans

Project SEKAI 翻译校对系统。生产发布合同是 NEXT 自有的 standalone 单镜像：SQLite 单一数据源、CDN 友好的文件分发与实时校对均由一个 Go 服务提供。

## 架构

```
v2/
├── server/                 Go 后端 (module moesekai/server, Go 1.25)
│   ├── main.go             组装依赖、启动 HTTP + 后台任务
│   ├── cmd/migrate/        旧 translations/ → SQLite 迁移工具（含无损校验）
│   └── internal/
│       ├── db/             SQLite 连接 + schema（modernc.org/sqlite，纯 Go）
│       ├── model/          共享类型 + 分类定义
│       ├── store/          翻译 + 活动剧情 CRUD（顺序保留）
│       ├── legacy/         旧文件加载（含 virtualLive 损坏数据恢复）
│       ├── files/          从 DB 再生成兼容格式 JSON
│       ├── filesvc/        /files/* 内存缓存服务（ETag + Cache-Control）
│       ├── publiclyricsbundle/ 已验收 Public Lyrics v3 只读冷启动基线包
│       ├── embeddedlyricsseed/ 私有 canonical 700 后台编辑 seed（不公开 serve）
│       ├── searchindex/    search-index.json 生成
│       ├── config/         设置存储（AES-GCM 加密密钥）+ env 种子
│       ├── auth/           JWT + bcrypt + RBAC（admin/editor）
│       ├── translator/     CN 同步 + AI 翻译 + 活动剧情同步
│       ├── upstream/       current_version.json 轮询 + 内置 git 镜像
│       ├── backup/         S3 + GitHub 备份/恢复
│       ├── importer/       备份恢复共享导入逻辑
│       ├── collab/         Ygo/Yjs 歌词协作、短票据、checkpoint 与 epoch fencing
│       ├── sse/            Server-Sent Events 实时推送
│       └── api/            HTTP 路由 + handlers
└── web/                    Next.js 15 控制台（仿 claude.ai，无 emoji）
    └── src/
        ├── app/            页面（登录/控制台/管理设置）+ 设计系统
        ├── components/     LoginPage, Console
        └── lib/            api.ts（类型化客户端）, sse.ts, labels.ts
```

## 路径分层（CDN 友好）

| 路径 | 用途 | 缓存 |
|------|------|------|
| `/files/*` | 公开翻译数据（兼容旧格式，给 pjsk.moe） | `public, max-age, stale-while-revalidate` + ETag |
| `/api/*` | 控制台 API（JWT） | `no-store` |
| `/yjs/lyrics/{musicId}` | Yjs 歌词协作 WebSocket（一次性短票据） | `no-store` |
| `/sse` | 实时事件推送 | 流式，不缓存 |

公开文件与旧系统**完全格式兼容**——消费端（pjsk.moe）零改动。

## 数据流

1. **编辑真源**：SQLite。所有翻译、活动剧情、来源标记、ID 追踪都存在 DB。
2. **公开分发**：DB 变更（去抖）后再生成 `/files/translation/*.json` 与 `search-index.json`。
3. **私有后台歌词 seed**：`next-production` 镜像另内嵌 canonical 700 的私有编辑 seed（归档 SHA-256 pin 见 `contracts/public-lyrics/baseline.json`）。生产启动在 settings/admin 环境种子之前检查目录；空目录会明确 defer 以兼容既有首次 bootstrap，任何非空目录都必须严格核对 700 首 ID、日文标题与 `catalog-identity-v2` fingerprint，再用一个事务只补完全没有歌词 ownership 的曲目：652 首写入 native plural source-v3、music 795 保持 legacy 可编辑、47 首写明确 availability；已有 legacy/source/recovery availability 一律 `preserved_existing`，不覆盖账号、设置、普通翻译、剧情、审核或 publication。重启只做精确 replay 验证并产生 0 新写入，707 或任意目录漂移 fail-closed。该 archive 不包含 raw evidence、provider payload 或 producer DB，也不会被公开路由 serve。
4. **Public Lyrics 公开发布**：公开歌词由数据库发布决定——控制台点「发布」即可让新歌出现在 `/files/translation/lyrics/index.json` 与 `/files/translation/lyrics/music_<id>.json`（含 `v2/{locale}/` 镜像），不需要改任何文件或重新打包镜像；「取消发布」同样即时生效，即使该曲存在于内嵌包内，也会从索引与详情路由中移除。生产 standalone 镜像内嵌的已验收 Public Lyrics v3 只读包只是冷启动基线：每次 files-service projection rebuild 先完成 SQLite 投影，再由数据库发布覆盖同名条目；包加载失败时 rebuild 不中断，projection status 记为 degraded 并只服务数据库发布内容。公开包只含 `index.json` 与 `music_<id>.json`，不含 manifest、receipt、producer DB、evidence 或任何私有输入；归档 SHA-256 与数量 pins 统一记录在 `contracts/public-lyrics/baseline.json`。
5. **来源优先级**：官方 CN 同步只尊重 `pinned`（控制台「锁定」）。非空官方 CN 文本会覆盖 `human`、`llm`、`unknown` 与既有 `cn`，空的官方值则保留现有非空文本；mysekai 的 `tag` → `flavorText` 镜像不检查来源，同名 `tag` 的文本与来源会覆盖 `flavorText`，锁定 `flavorText` 也挡不住（只能锁定对应的 `tag` 条目）。人工译文要想不被下一次官方同步覆盖，必须先锁定为 `pinned`。

歌词首次普通保存可以不填 `sourceUrl`，也可以填写有效的非托管外部参考链接；产品托管的 Vocaloid Wiki 精确 origin 只能通过服务端验证 preview/import 路径写入完整 page/revision/SHA1/fetch identity，不能仅粘贴 Wiki URL 绕过验证。历史上已经存在的仅 URL Wiki 草稿仍可继续编辑翻译，但其 URL、来源 identity 和日文来源结构必须保持不变。固定 page/revision/SHA1 只证明保存内容与某次抓取的修订一致，不证明转载、翻译或发布获授权；私有 `sourceNote`、`licenseNote` 与公开 `attribution` 都是 operator-authored metadata，系统不会把它们当作权利或许可证明。

歌词维护约定：SQLite/API 的 `LyricLine`、`LyricSegment`、`LyricRubySpan` 是编辑数据模型；Web 的分段、合并、ruby 同步行为统一放在 `web/src/lib/lyrics-segmentation.mjs`，行/分段/ruby 控件统一由 `web/src/components/lyrics/LyricsLineEditor.tsx` 渲染，页面容器不复制这套 JSX。来源 parser 只能增加通用规则，并同时添加固定 revision fixture 或合成的 fail-closed 安全测试，禁止按 music ID/歌名分支。Parser 更新后的覆盖重算使用 `lyrics-preflight -resume-incomplete-codes unsupported_format,missing_lyrics` 精准重跑确定性不完整项；该路径复用旧报告中固定的 page/revision/SHA1，跳过候选重搜，只重新抓取并解析同一 revision。`ambiguous_source` 不在白名单内，不能默认选择第一套歌词。latest schema 测试应从 `migrations` 列表读取，只有历史迁移版本与 checksum 保持显式固定。Sekaipedia 的乐曲 ID→页面标题映射与词曲作者别名是数据库数据（schema v36，种子为此前编译进二进制的同一份映射），在「管理设置 → 歌词来源映射（Sekaipedia）」增删改，校验通过后立即生效、无需重启；固定的 `List of songs` 索引 revision 仍编译在二进制中。v19 会删除夹在有效文本中的 legacy 空 segment；若某行原来只有空 segment 但日文非空，则只把最小 position 的一项确定性修复为整行日文，其余空项删除；迁移后每个持久 segment 都有非空且按 text 精确复现 segment 的 ruby。旧 v18 writer 省略 `ruby_json` 的 insert 会在语句结束前自动规范成单个 exact-text span。

Phase 1 自动歌词发现默认关闭，首次配置可用 `LYRICS_DISCOVERY_ENABLED=true` 启用；种子写入后以认证设置 `lyrics_discovery.enabled` 为准。v13-v15 的 Phase 2 私有来源流水线现已实现：目录证据必须明确给出 role-bound credits 与唯一 full 目标，game-size 仅作为关联证据，缺失版本或 short/preview/partial/cover/medley 信号均不会自动抓取。唯一候选会在 discovery 完成事务中排入带完整版本化候选身份（page/revision/SHA1、page title、canonical `oldid` URL、排序 categories）的固定修订任务；多候选或目录歧义进入私有审核队列。独立 fetch worker 由 `LYRICS_FETCH_REVISION_ENABLED=true` / `lyrics_discovery.fetch_revision.enabled` 单独控制且同样默认关闭，精确核对完整候选身份，并在当前 title/URL/categories（含限制分类）相对审核快照漂移时先行拒绝，之后才原子保存有 2 MiB 上限的不可变 raw wikitext Artifact、版本化 analysis/association/job output 和三门审核项。两个 worker 只在 TCP bind 成功后启动，停机按 Drain → Cancel → Wait 后再关闭 SQLite；各自的 lease/job-timeout/idle/retry 环境变量只接受有界正整数毫秒。

Phase 2 的规范 admin API 固定为 `GET /api/admin/lyrics-source-reviews`、`GET /api/admin/lyrics-source-reviews/detail?reviewId=`、`PUT /api/admin/lyrics-source-reviews/decision`、`PUT /api/admin/lyrics-source-reviews/candidate-selection`、`POST /api/admin/lyrics-source-reviews/import`。它们仅接受 bearer JWT 管理员，PUT/POST 仅接受 JSON，使用 expected version CAS 与 actor-scoped idempotency；同一 actor 的幂等键在 single/candidate 决定账本与 batch parent 账本之间共享，只有同请求种类且 payload 完全一致才 replay，跨种类/不同 payload 或 batch 派生 child key 碰撞都会在写入前确定性返回 `idempotency_conflict` 并保持整批零应用。Console 的独立“歌词来源审核”页可选择固定候选，Artifact 则只需一次整体通过或拒绝。新的 `overall` 决定会在一个事务和一个版本增量内原子设置底层 `identity`/`source_use`/`parse` 三项兼容状态；旧的逐门决定历史仍可读取，仍处于 pending 的旧部分审核项也可用整体决定完成。冻结的列表响应只含 review identity/state、music/title、catalog fingerprint、reason、三项兼容状态、version/priority/timestamps；详情只增加候选固定身份，或安全的 page/revision/SHA1/categories/fetch facts、机器 policy/outcome/evidence、提取行、associations 与决策历史；single/candidate mutation 只回显 review/gate state/version/replayed，batch 成功响应严格为顶层 `{items,replayed}`，每个 item 严格为 `{reviewId,state,version}`。raw wikitext 及内部 artifact/analysis/extracted-line hashes 不会进入 API、列表、日志、公共文件、SSE 或 Git/S3 content backup；共享 restore 保留私有表并只把目录 fingerprint 已过期的 pending review 标记为 superseded。

已注册的 `POST /api/admin/lyrics-source-reviews/import` 是审核通过后的显式导入入口，请求闭集为 `{reviewId}`。它在 first-save 事务内重新核对 approved/gate 状态、policy、完整当前 catalog target 分组/fingerprint、不可变来源 identity、结构化提取 digest、ruby/segment 与 performer 投影，只创建 revision 1 的私有可编辑草稿，`zh-CN`/`en-US` 为空且不会发布。来源已声明但不在封闭游戏角色目录中的人声/外部歌手保留在来源证据中；存在 catalog fallback 时使用合法目录 ID，只有 `outside_character` 时则保存具体 `[]`，不伪造 ID，并由发布校验继续阻断直到编辑者处理。相同文档重放返回 `changed:false`；已有非相同歌词文档返回 `lyrics_already_saved`。单纯审核/候选决定仍不保存草稿，只有显式 import 成功提交后才发送普通私有内容变更通知。

本地 staging 链与生产 API 分离。`lyrics-stage` 的 report/manifest 仍固定 catalog contract v18，但只在完整必需列、`catalog-identity-v2` policy 与逐行 fingerprint 重算都一致时接受独立不可变的 runtime schema v18、v19 或 v20 SQLite snapshot；未来 schema 默认拒绝。新增 `lyrics-import-stage` 仅供离线本地 operator 使用，不会复制进生产镜像，也不会由服务端调用。命令要求 manifest/DB/backup/receipt 的绝对路径、已验证 backup SHA-256、审计 operator 和 `-confirm-local-offline`，会拒绝 `MOESEKAI_PRODUCTION`、取得同一 single-instance DB lock、固定 inode、拒绝 SQLite sidecar，并用 `O_EXCL` 创建私有 receipt。导入时再次验证 closed manifest/digest、完整当前目录 generation、fingerprint/target/association、page/revision/SHA1 URL、performer、空中英文翻译和 ruby/segment。不可变 manifest 会原样保留来源 performer legend；运行时数值投影只接受封闭的游戏角色目录 ID。已在 legend 声明但无法映射的人声/外部歌手会在存在时使用所选 catalog vocal fallback；若 catalog 只有 `outside_character`，私有草稿保存具体的空 `performerIds` 数组而不伪造会碰撞的游戏角色 ID，并继续由发布校验阻断，等待编辑者显式处理。整批一个事务，完全相同的已有草稿可幂等重放，非相同已有文档冲突，任一项失败则整批回滚。schema-v2 manifest 在每个 `source` 中保存该项实际固定修订抓取的 `fetchedAt`，导入时逐项写入 `sourceFetchedAt`；preflight `generatedAt` 只保留报告生成时间，不再代替来源抓取时间。示例：

```bash
cd server
go run ./cmd/lyrics-import-stage \
  -manifest /absolute/private/staging.json \
  -db /absolute/offline/moesekai.db \
  -backup /absolute/offline/moesekai-before-import.db \
  -backup-sha256 '<64-lowercase-hex>' \
  -receipt /absolute/private/import-receipt.json \
  -operator '<local-operator>' \
  -confirm-local-offline
```

活动剧情同步还会把每个 JP episode 的完整原始 Scenario 规范化为确定性紧凑 JSON，并把 SHA-256 存入无 legacy 父外键的 side table。`GET /api/event-story/episode-snapshot` 为已登录编辑器提供单事务、revision-safe 的 episode 翻译片段与 SekaiText 兼容 `sourceTalks`；原始 Scenario 只进入认证 API 和私有增量备份，绝不进入现有公共剧情 JSON。

## 实时性

普通内容通知、editor gate、同步/翻译进度和恢复通知继续走现有事件通道；歌词正文协作独立使用浏览器 `yjs` + `y-websocket` 和同一 Go 进程内的 `github.com/reearth/ygo`。服务端不需要 Node、V8 或独立协作进程。每首歌的 `Y.Doc` 以 `lyrics` 为根，正文和结构分别使用嵌套的 `Y.Text`、`Y.Array`、`Y.Map`，不是把整份歌词塞进一个 JSON 字符串。

`segments` 与 `ruby` 的数组项在 Y.Doc 内携带 `__yjsId`、`__yjsGeneration` 和可选 `__yjsOrigin`，用于识别两个客户端从同一结构基线并发 split 的情况；这些字段只属于协作协议，物化为歌词 DTO 时会剥离，不会写入规范化歌词表或公共 JSON。完全没有这些字段的旧房间仍可读取；混合新旧身份、残缺身份、重复 ID，或同一 origin 出现多个 generation 会被判定为结构冲突。客户端会立即停止自动重连、保留最近的权威歌词只读并要求重新加载，checkpoint 也会拒绝该文档，而不会猜测合并出错误歌词。

连接流程固定为：浏览器携带长期会话 JWT 和已加载的 producer-state proof，以 `Authorization: Bearer` 调用严格的 `POST /api/editor/v1/lyrics/{musicId}/collab-ticket`；服务端返回严格 DTO `{ticket,room,expiresAt}`；浏览器随后连接 `/yjs/lyrics/{musicId}?ticket=...`。ticket 有效期 45 秒、只允许一次升级尝试，并绑定用户、角色/token generation、music ID、协作 epoch 与 atomically accepted editor-gate 快照。已经建立的 Yjs 写连接会一直占用 editor-gate shared admission；producer 发布 running 状态后先关闭这些房间并等待连接释放，再执行权威替换。Yjs 协作 WebSocket URL 绝不携带长期 JWT；响应中的 `room` 是服务端诊断身份，客户端仍以 URL 的 `musicId` 和 ticket 完成授权，服务端再把它改写为内部 `lyrics-{musicId}-e{epoch}` room。断线时销毁旧 provider，按有界退避重新取票，不能重放旧 ticket 或把旧 epoch 的离线 `Y.Doc` 合并进新房间。

`y-websocket` 3.x 会在约 30 秒未收到服务端消息时主动重连。单人房间通常没有别人的 update/awareness，因此客户端固定设置 `resyncInterval: 15_000`；周期 SyncStep1 会得到服务端 SyncStep2，避免健康连接被 watchdog 当成静默连接。界面只在首个 `sync(true)` 后解除只读，单纯 TCP/WebSocket `connected` 不代表文档可编辑。

协作 update 只是可恢复草稿，不是“已保存”。用户显式保存时调用 `POST /api/editor/v1/lyrics/{musicId}/checkpoint`；服务端重新读取权威歌词、校验基础 revision/authority hash 和 editor gate，再把 Yjs 文档物化为既有严格歌词模型，经现有校验写入 SQLite。权威歌词 mutation、协作 snapshot/epoch 与 checkpoint ledger 使用同一个 SQLite 事务，任一写入失败都会整体回滚；首次保存还会先冻结并排空旧 room，再在该事务内 reseed 新 epoch。SQLite 的规范化歌词、revision、不可变来源资料和 publication projection 继续是备份、发布与 API 的唯一权威。

Ygo 资源门禁为代码固定的安全默认值：全局 50 条连接、每房间 10 个 peer、最多 256 个房间（最多 128 个常驻）、raw update 与合并文档上限均为 8 MiB，WebSocket message 上限为 8 MiB + 64 KiB 以容纳 Yjs framing、最多 100,000 个 pending item、每连接每秒 20 条消息且 burst 40、peer 写队列 256、每房间 awareness 256 KiB/256 client、10 秒握手、90 秒 awareness 过期、5 分钟空闲房间回收、持久化 coalesce 最长等待 1 秒，并每 100 次成功 flush compact 一次。当前不提供环境变量绕过这些上限。

## 更新检测

默认轮询 `https://metadata.pjsk.moe/jp/versions/current_version.json` 的 `dataVersion`，并发竞速 GitHub Raw、Fastly、Gcore 和 jsDelivr 救援源，任一成功即取消其余请求。JP/CN masterdata 和 JP/CN 剧情资源均支持主源、备用源与 `{repo}` / `{branch}` 模板；masterdata 主源响应过慢时会提前启动备用源。默认 JP 剧情源为 `assets.unipjsk.com`（已移除持续返回 HTTP 525 的 snowyassets 默认链路）。同步会有限并发拉取分类及剧情资源，默认并发数为 4，可通过 `UPSTREAM_FETCH_CONCURRENCY` / `upstream.fetch_concurrency` 调整。watcher 会记录实际使用源、上次成功时间和连续失败次数，并按 `Retry-After` 或本地退避冷却 429。完整配置见 `.env.example`，可选维护本地 git 镜像（`UPSTREAM_USE_GIT=true`）。

## 备份 / 恢复

每日自动 + 手动，两个独立目标：
- **GitHub**：`git clone/commit/push` 到指定仓库与分支
- **S3 兼容**：tar.gz 上传（内置 SigV4 签名，支持 AWS S3 / Cloudflare R2 / MinIO）

恢复从任一目标拉取并重新导入 SQLite。

应用内 Git/S3 内容备份不是完整数据库备份：它不包含用户、密码哈希、token generation、设置、审计记录或加密配置。生产环境必须另行使用 SQLite 在线备份语义生成完整快照，执行 `PRAGMA integrity_check`，加密后传到独立的 off-host 存储，并定期做恢复演练。不能在 WAL 活跃时只复制 `moesekai.db` 主文件。

备份中的 `translations/` 以 `Generator.WriteAllContext` 生成的 legacy category/event restore projection 为基础；`materializeBackupPayload` 再从同一个 SQLite snapshot 明确写入与该快照 `PublishedLyricsJSON` 字节完全一致的 `translations/lyrics/index.json` 和 `translations/lyrics/music_<id>.json` 数据库投影归档，而不改变通用 legacy generator 的输出语义。published lyrics 的草稿、私有来源资料和 publication snapshot 另存于 `translation-content/lyrics.json`，用于恢复 SQLite 自身的可编辑状态。镜像内嵌的 Public Lyrics 冷启动基线属于不可变公开 release content，不进入内容备份、不会被 restore 写入数据库，并会在 restore 后的下一次 files-service rebuild 中继续覆盖对外歌词路径。该 materialization 与备份 push 都不是部署、静态站点发布或 CDN 同步；v2 locale projections 与 search indexes 仍不进入该归档。

增量备份继续使用 `translation-content` schemaVersion 1；`event-stories.json` 追加规范化 Scenario 与独立 `scenarioCount`，旧 `count` 语义不变。旧备份不含 Scenario 时会显式清空该 side table，恢复前会校验 SHA 与 event/episode/scenario 父身份并保持整笔事务原子。

NEXT standalone 生产镜像的手动发布、精确 push CI 门禁、Cosign OIDC 签名、自定义 predicate 与环境配置见 [`STANDALONE_RELEASE.md`](STANDALONE_RELEASE.md)。生产回滚（镜像 / 制品、数据库迁移兼容、密钥与验证）见 [`ROLLBACK_RUNBOOK.md`](ROLLBACK_RUNBOOK.md)。

## 多用户与权限

- **admin**：管理用户、改设置、备份/恢复、触发同步，以及全部校对操作。
- **editor**：仅校对操作。

## 运行

### 本地开发

```bash
# 1. 迁移旧数据到 SQLite
cd server
go run ./cmd/migrate -src ../../translations -db ./data/moesekai.db

# 2. 启动后端
JWT_SECRET=$(openssl rand -hex 32) MOESEKAI_MASTER_KEY=dev ADMIN_USER=admin ADMIN_PASSWORD='local-admin-password' go run .

# 3. 启动前端（另开终端）
cd ../web
npm ci
npm run dev          # http://localhost:3000，自动代理 /api、/ws、/yjs 到 :8080（可用 BACKEND_ORIGIN 修改）
```

### Docker

Dockerfile 内置了供 Zeabur 等直接从源码构建的平台使用的审核基础镜像默认值：Node、Go 和 runtime 镜像名及其完整 sha256 digest 均已固定。官方 GitHub Actions CI/release 仍会显式传入仓库批准的变量，并继续对镜像名、digest 和最终候选制品执行独立校验；这些默认值不替代受保护发布链路。

```bash
# 发布构建必须使用经审核且带 sha256 digest 的三个基础镜像。runtime 镜像需预装
# 固定版本的 git、CA 证书与 tzdata；Dockerfile 不会在构建时安装可变软件包。
docker build \
  --target next-production \
  --build-arg NODE_IMAGE='node:20.19.4-alpine3.22' \
  --build-arg NODE_IMAGE_DIGEST='<approved-64-hex-digest>' \
  --build-arg GO_IMAGE='golang:1.25.1-alpine3.22' \
  --build-arg GO_IMAGE_DIGEST='<approved-64-hex-digest>' \
  --build-arg RUNTIME_IMAGE='<approved-runtime-with-git>' \
  --build-arg RUNTIME_IMAGE_DIGEST='<approved-64-hex-digest>' \
  --build-arg VERSION='<release>' --build-arg VCS_REF="$(git rev-parse HEAD)" \
  -t nexttrans .
docker run -p 8080:8080 -v nexttrans-data:/data \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
  -e JWT_SECRET=... -e MOESEKAI_MASTER_KEY=... \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD=... \
  nexttrans
```

`ADMIN_PASSWORD` 只应在受控首次启动或管理员恢复时临时注入；确认管理员和持久卷身份后应从长期容器环境移除。服务默认监听 `0.0.0.0:8080`；`CONSOLE_ORIGIN` 未配置时使用 `*`，适配 Zeabur 等平台分配的生产域名，也可在需要时覆盖为一个精确的 http(s) origin。公开 `/files/*` 与 `/translation/*` 始终返回通配 CORS。生产容器应使用镜像默认的 `DB_PATH=/data/moesekai.db`、`DATA_DIR=/data` 与精确 `TZ=UTC`（`.env.example` 也按容器路径编写）；本地 `go run` 才覆盖为 `./data/...`。生产启动还会把 Go 的进程本地时区固定为 UTC，不依赖基础镜像的 `/etc/localtime`。

镜像只运行一个 Go 进程（默认 `:8080`）：同时提供静态控制台、`/api`、`/sse`、`/ws`、`/yjs`、`/files` 与 `/translation`。生产必须保持单实例，并使用 `Recreate`（先停止旧实例，再启动新实例），不能让两个 SQLite writer 或两个进程内 editor gate 同时对外服务。仓库默认不附带 `seed-translations/`；如需首次迁移，应在上线前显式提供并验证种子，迁移或无损 round-trip 校验失败会终止容器启动。

运行阶段固定使用非 root 的 `65532:65532`；`/app`、二进制和控制台全部由 root 持有且不可写，只有持久化 `/data` 与显式 `/tmp` 可写。挂载自定义宿主数据目录时必须预先授予该 UID/GID 写入权限。CI 所需的三个 digest 与 runtime 镜像名由同名 repository variables 提供，缺失或非 64 位小写十六进制值会使构建失败。

Dockerfile 的默认最终目标是 `next-production`，继承自 `standalone`。最终阶段使用链接时写入 `next-production` profile 的专用服务端二进制；即使部署层尝试覆盖环境，它也拒绝把 `MOESEKAI_PRODUCTION=true` 改为空值或 `false`。镜像同时永久设置服务端约定的 `WORKSPACE_MODE=disabled`、`WEB_DIR=/app/web`、`DB_PATH=/data/moesekai.db`、`DATA_DIR=/data` 与 `TZ=UTC`；`WORKSPACE_WEB_DIR`、`WORKSPACE_MANIFEST_SHA256` 必须保持未设置，生产层若缺失或覆盖静态根、数据库路径、数据目录或时区，会在持久化目录被触碰前失败。构建上下文和 `/app` 中都没有外部 workspace。容器 entrypoint 会先执行 `moesekai-server --verify-runtime`，在任何持久化目录创建、权限修改或 seed migration 前拒绝残留/错误配置；正常 Go 启动仍会再次验证。`/workspace`、编码分隔符/反斜杠、有效或畸形的多层百分号编码、点段和其他规范化后进入 `/workspace` 的路径与方法均固定返回带安全头的 `404 no-store`，不会被 OPTIONS 预检或 ServeMux 规范化绕过，也不会回退到管理控制台。仅后端非生产 characterization 才显式使用 `--target runtime`。

生产发布只可从受保护的 `main` 手动 dispatch `Release NEXT standalone image`。只读 `prepare` job 会先选定同一 full SHA 的最新匹配 `push` CI attempt，并要求该 attempt 已成功完成；随后按 artifact ID 下载并校验候选/回滚 ZIP 的 GitHub digest、闭集内容、候选文件 SHA、rollback tar SHA/内部校验与 base digests。只有依赖这些固定输出的 `publish` job 受 `production` environment 审批保护，因此同 SHA 的后续 rerun 不能替换已审批输入。审批后与最终发 tag 前会再次确认线上 main tip；由于分支查询和 GHCR 写入无法原子化，操作员必须从审批到最终 tag 验证期间冻结 protected main 更新。

生产镜像只在该 push CI 构建一次并完成真实容器门禁；CI 将 exact `docker save` 候选、closed metadata/SHA256SUMS 与 rollback bundle 以 `<sha>-<attempt>` 命名上传 90 天。发布工作流在审批前后都按固定 artifact ID 下载两份制品，并将下载 ZIP 的 SHA-256 与 GitHub API digest 精确比较；审批后的 candidate 文件 SHA/base digests 与 rollback tar SHA/内部闭集校验都必须等于 prepare 阶段记录，才加载同一 image ID，再只做 retag/push 到 `staging-<run-id>-<attempt>`，绝不重新 build 或读取 release-time base variables。随后对 digest 强制执行 Cosign GitHub OIDC 签名、自定义 NEXT-only attestation、精确 `release-next.yml@refs/heads/main` identity 与 NEXT workflow SHA 校验；predicate 绑定 CI run、候选/回滚 artifact ID 与 archive digest、候选文件 SHA、rollback tar SHA、base digests 和最终 image digest。验证允许历史有效 statement 共存，但必须至少存在一份与当前完整 predicate 精确相等的 statement。全部成功后才发布唯一最终 tag `next-<next-full-sha>`；同 digest 重试可幂等通过，不同 digest 必须失败，不创建 `latest`、branch 或 SemVer tag。支持 GitHub artifact attestations 时可显式启用同一自定义 predicate 的额外 GitHub attestation；promotion job 不冒充 image builder，且发布工作流本身不修改运行中的服务。完整合同见 [`STANDALONE_RELEASE.md`](STANDALONE_RELEASE.md)。

初始 public projection 在受跟踪的后台 worker 中异步生成；生成完成前 `/healthz` 可用而 `/readyz` 返回未就绪。任何仍未发布的失败 generation（包括启动后失败）都会按有界退避重试到成功或停止；最新 generation 处于 `Pending` 且有 `LastError` 时 `/readyz` 返回 `503`，成功重试发布后恢复 `200`。收到 drain 后，除精确的 `GET`/`HEAD /healthz` 和 `/readyz` 外，新的 API、搜索、静态文件、SSE 与 OPTIONS 请求全部返回 `503`；已废弃的 `/workspace` tombstone 在生命周期 admission 之前仍固定返回 `404 no-store`。后台任务和 HTTP 请求结束后才关闭 SQLite。已进入的本地文件系统/SQLite projection 临界区无法安全强制取消，进程会等待其返回，这是保留的无界优雅停机风险。

## 配置

见 `.env.example`。`JWT_SECRET` 至少 32 字节，新增/重置密码为 12-72 字节。生产 `MOESEKAI_MASTER_KEY` 必须包含至少 32 字节随机 secret material；用 `openssl rand -base64 32` 生成一次，存入 secret manager，并在重启、恢复和回滚时保持稳定。standalone 镜像永久设置 `MOESEKAI_PRODUCTION=true` 与 `WORKSPACE_MODE=disabled`，并要求外部 workspace 目录/hash 变量保持未设置、master key 合格且数据库中至少一个 admin；全新数据库必须用 `ADMIN_PASSWORD` 或 `TRANSLATOR_ACCOUNTS` 完成 bootstrap。只有 `editor`/`admin` 是有效角色，数据库存在其他角色会阻止启动。密钥项（LLM key、备份凭证）在 DB 中以 AES-GCM 加密存储，由 `MOESEKAI_MASTER_KEY` 派生密钥。env 变量仅在**首次启动**作为种子写入；之后管理设置页是唯一真源；设置更新拒绝未知键，`backup.daily_hour` 只接受规范十进制整数 `0` 到 `23`，整个 patch 原子提交。

## 测试

```bash
cd server && go test ./...
go test -race ./...
go vet ./...
cd ../web && npm ci && npm test && npm run typecheck && npm run lint && npm run build
cd .. && ./scripts/verify-release.sh
```

迁移工具自带无损往返校验：导入后从 DB 读回，逐条比对文本、来源、ID、活动剧情每行及其顺序。
