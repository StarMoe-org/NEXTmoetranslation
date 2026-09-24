# NextTrans

Project SEKAI 翻译校对系统。生产发布合同是 NEXT 自有的 standalone 单镜像：SQLite 是唯一编辑真源，一个 Go 进程同时提供控制台静态页、`/api`、`/sse`、`/yjs` 协作 WebSocket，以及 CDN 友好的 `/files/*` 与 `/translation/*` 公开文件（与旧系统格式完全兼容，pjsk.moe 侧零改动）。翻译词条、活动剧情、歌词草稿与发布状态都在同一个数据库里；公开 JSON 由数据库投影再生成，而不是手工维护的文件。当前数据库 schema 版本为 v39（v37 放宽 `song_lyrics_source_artifacts` 的来源检查，允许 `https://projectsekai.fandom.com`；v38 只新建恢复台账接管表 `lyrics_recovery_takeovers`；v39 `side_stories` 只新建卡牌剧情与区域对话的四张表；迁移只能前进，部署前检查见 [`ROLLBACK_RUNBOOK.md`](ROLLBACK_RUNBOOK.md)）。

```
NEXTmoetranslation/
├── server/                     Go 后端（module moesekai/server，Go 1.25）
│   ├── main.go                 进程入口；启动装配拆在 startup.go / wiring.go / http.go / seed.go / shutdown.go / env.go / operational.go
│   ├── cmd/migrate/            旧 translations/ → SQLite 迁移工具（含无损往返校验）
│   ├── offline/                离线 operator 工具的嵌套 module（moesekai/server/offline，replace moesekai/server => ../）
│   │   ├── cmd/                lyricsctl、lyrics-stage、lyrics-import-stage、lyrics-preflight、lyrics-validate、lyrics-recovery* 等 12 个命令
│   │   └── internal/           16 个离线专用包；生产二进制不 require 本 module
│   └── internal/
│       ├── db/                 SQLite 连接 + 迁移（modernc.org/sqlite，纯 Go）
│       ├── model/ store/ importer/ legacy/    共享类型、翻译与歌词 CRUD、备份导入、旧文件加载
│       ├── files/ filesvc/ searchindex/       公开 JSON 生成、/files 内存缓存服务、search-index.json
│       ├── publiclyricsbundle/                已验收 Public Lyrics v3 只读冷启动基线包
│       ├── embeddedlyricsseed/                私有 canonical 700 后台编辑 seed（不公开 serve）
│       ├── config/ auth/ editorgate/          设置（AES-GCM）、JWT + RBAC、单实例编辑门禁
│       ├── singleinstance/ lifecycle/ httpx/ workspaceverify/
│       ├── translator/ upstream/ backup/      CN 同步与 AI 翻译、上游版本轮询、Git/S3 备份
│       ├── lyricssource/ lyricsdiscovery/ lyricscontract/ lyricsperformers/ lyricsprovideroutcome/
│       ├── collab/ sse/                       Yjs 歌词协作（github.com/reearth/ygo）、SSE 推送
│       └── api/                               路由表 routes.go + handlers，装配在 server_wiring.go
└── web/                        Next.js 15 控制台（静态导出，无 emoji）
    └── src/
        ├── app/                页面与设计系统
        ├── components/
        │   ├── console/        Console.tsx 的状态与区块：useConsoleRealtime、useConsoleEntries、TranslationEntryWorkspace、ProducerOperationsShell
        │   ├── lyrics/         LyricsEditor.tsx 的状态与区块：useLyricsDocumentLoader、useLyricsPersistence、LyricsDocumentView 等
        │   └── admin/          LyricsProviderTargets.tsx：「管理设置 → 歌词来源映射（Sekaipedia）」面板
        └── lib/                api.ts、sse.ts、yjs-lyrics.ts、labels.ts 与纯函数 .mjs 模块
```

歌词来源映射面板对应 schema v36 的 `lyrics_provider_page_targets` 与 `lyrics_provider_contributor_aliases`：Sekaipedia 的乐曲 ID→页面标题映射和词曲作者别名从二进制移入数据库，管理员通过 `GET /api/admin/lyrics-providers/sekaipedia/targets` 与 `PUT`/`DELETE /api/admin/lyrics-providers/sekaipedia/targets/{musicId}` 增删改，整表校验通过后热替换来源注册表，无需重启；固定的 `List of songs` 索引 revision 仍编译在二进制中。

## 怎么跑

### 本地开发

```bash
# 1. 迁移旧数据到 SQLite（可选，仅首次）
cd server
go run ./cmd/migrate -src ../../translations -db ./data/moesekai.db

# 2. 启动后端
JWT_SECRET=$(openssl rand -hex 32) MOESEKAI_MASTER_KEY=dev ADMIN_USER=admin ADMIN_PASSWORD='local-admin-password' go run .

# 3. 启动前端（另开终端）
cd ../web
npm ci
npm run dev          # http://localhost:3000，把 /api、/sse、/yjs、/files 代理到 :8080（可用 BACKEND_ORIGIN 修改）
```

### 生产

生产是 Dockerfile 的默认最终目标 `next-production`，永久设置 `MOESEKAI_PRODUCTION=true`、`WORKSPACE_MODE=disabled`、`WEB_DIR=/app/web`、`DB_PATH=/data/moesekai.db`、`DATA_DIR=/data`、`TZ=UTC`，以非 root `65532:65532` 运行只读根文件系统 + 可写 `/data`：

```bash
docker run -p 8080:8080 -v nexttrans-data:/data \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
  -e JWT_SECRET=... -e MOESEKAI_MASTER_KEY=... \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD=... \
  nexttrans
```

`JWT_SECRET` 至少 32 字节，生产 `MOESEKAI_MASTER_KEY` 必须是至少 32 字节的随机 secret 且跨重启/恢复/回滚保持稳定。`ADMIN_PASSWORD` 只在受控首次启动或管理员恢复时临时注入。env 变量只在**首次启动**作为种子写入，之后管理设置页是唯一真源。完整变量表见 `.env.example`；镜像构建、digest pin、签名与发布链路见 [`STANDALONE_RELEASE.md`](STANDALONE_RELEASE.md)，回滚见 [`ROLLBACK_RUNBOOK.md`](ROLLBACK_RUNBOOK.md)，主站与 operator 依赖的接口合同见 [`PRODUCTION_CONTRACT.md`](PRODUCTION_CONTRACT.md)。

生产必须保持单实例并使用 `Recreate`（先停旧再起新），不能让两个 SQLite writer 或两个进程内 editor gate 同时对外服务。

### 离线 operator 命令

全部离线命令都在嵌套 module 里，必须先 `cd server/offline`；生产镜像不包含它们，服务端也不会调用：

```bash
cd server/offline
go run ./cmd/lyrics-import-stage \
  -manifest /absolute/private/staging.json \
  -db /absolute/offline/moesekai.db \
  -backup /absolute/offline/moesekai-before-import.db \
  -backup-sha256 '<64-lowercase-hex>' \
  -receipt /absolute/private/import-receipt.json \
  -operator '<local-operator>' \
  -confirm-local-offline
```

各离线命令对数据库 schema 的要求不同：`lyrics-stage` 要求迁移历史连续、止于 v18 至 v39；`lyrics-import-stage`、`lyrics-recovery-import` 与 `lyrics-recovery-public-candidate` 要求连续的 v27 至 v39（v39 只加卡牌剧情与区域对话的表，已审阅为兼容，v40 起拒绝）；`lyrics-preflight` 要求目录库正好是 v18；`lyrics-catalog-filter` 只读最高版本号，不设上限，也不检查连续性。新的 schema 版本要先审阅兼容性，再提高这些上限。只要有一首歌被整曲文档接管（v38 的 `lyrics_recovery_takeovers`），或者有一首歌的 source 文档归编辑器所有（用 `PUT /api/editor/v1/lyrics/document` 发布过的歌、迁移 v32 写入的歌曲 682、内嵌编辑器 seed 写入的歌，见 `refuseRecoveryItemsForEditorOwnedSongs`），`lyrics-recovery-import` 就拒绝新的恢复批次，因为批次必须覆盖整个曲库。v32 写入过歌曲 682 的数据库从一开始就是这样。重放已导入的批次不受影响。

### 测试

```bash
cd server && go test ./... && go vet ./...
cd offline && go test ./... && go vet ./...
cd ../../web && npm ci && npm test && npm run typecheck && npm run lint && npm run build
cd .. && ./scripts/verify-release.sh
```

## 来源优先级真相

官方 CN 同步**会覆盖**人工译文：非空的官方 CN 文本会写掉来源为 `human`、`llm`、`unknown` 与既有 `cn` 的条目，只有 `pinned` 受保护；空的官方值则保留现有非空文本。这是有意行为，不是待修的缺陷——控制台对官方条目明确提示「保存后来源变为人工，下次官方同步仍会覆盖这条译文；需要长期保留请用『锁定保存』」，编辑器点「锁定保存」把条目写成 `pinned` 即可长期保留。

mysekai 的 `tag` → `flavorText` 镜像是另一条独立规则：同名 jp key 的 `tag` 文本与来源会直接覆盖 `flavorText` 的文本与来源，不检查 `flavorText` 自己的来源，因此锁定 `flavorText` 挡不住，只能锁定对应的 `tag` 条目。

编辑器接受的来源词表固定为 `cn`、`human`、`pinned`、`llm`、`unknown`，其他值在落库前返回 `400`。

## 发布怎么工作

**写入门禁。** 内容写入走 `/api/editor/v1/*`。为 agent 脚本重新挂载了三条旧路径：`PUT /api/entry`、`PUT /api/lyrics/save`、`POST /api/lyrics/publish` 分别与 `/api/editor/v1/entry`、`/api/editor/v1/lyrics/save`、`/api/editor/v1/lyrics/publish` 共用同一个鉴权包装与处理函数；其余六条无版本号写路由（`PUT /api/category/batch`、`POST /api/lyrics/translation-editions`、`POST /api/lyrics/unpublish`、`PUT /api/event-story/update`、`POST /api/event-story/promote-human`、`POST /api/backup/push`）仍返回 JSON 404。内容写入的 `X-Moe-Loaded-Producer-State: <instanceId>:<revision>:<completedGeneration>` 头是可选的：不带时按宽松门禁处理（编辑准入加共享内容锁，producer 运行中返回 `409`）；带了就必须与所编辑文档加载时的门禁状态一致——重复或畸形返回 `400`，producer 正在运行、revision/generation 过期或进程重启导致 instance 不匹配返回 `409` 并回带当前 producer 状态。只有 `POST /api/editor/v1/lyrics/{musicId}/collab-ticket` 与 `POST /api/editor/v1/backup/push` 仍强制该头，缺少返回 `428`。`PUT /api/editor/v1/lyrics/document`（管理员）一次提交整首歌词文档并直接发布。歌曲已存有或正在提供歌词时必须带 GET 给出的 `expectedRevision`；`dryRun` 在事务里执行与正式提交相同的写入后回滚，所以正式提交会遇到的错误它同样会返回；它在 `changes` 里列出将要发生的改动。有多个译本的歌用 `translationEditions`、`zhEditions`、`editionCredits` 携带全部译本，原样提交导出结果不会丢掉任何译本。恢复导入台账里的歌曲在首次发布时被接管，台账各行保持不变。同一路径的 `GET ?musicId=<id>`（编辑即可，只读，不需要 producer 头）把公开站当前提供的歌词转成可以直接提交的请求体，并用 `warnings` 列出请求格式装不下的内容；加 `&from=database` 则读数据库里可编辑的版本。`POST /api/editor/v1/lyrics/document/takeover`（管理员）把公开站正在提供的歌原样转成可编辑文档，控制台的「转为可编辑」按钮调用的就是它。agent 调用方式（含 `gachaInfo` 词条的写法）见 [`contracts/editor-api/README.md`](contracts/editor-api/README.md)。宽松门禁写路由 `POST /api/admin/lyrics-source-reviews/import`（SekaiText-Moe 仍在用）保留，它的严格双胞是 `POST /api/editor/v1/admin/lyrics-source-reviews/import`。`/sse` 连接建立后的第一帧固定是 `gate.status`，因此新开的标签页无需额外轮询就知道当前能不能写。

**歌词公开投影。** 镜像内嵌的已验收 Public Lyrics v3 只读包只是冷启动基线（归档 SHA-256 与数量 pin 统一记在 `contracts/public-lyrics/baseline.json`）。每次 files-service 投影重建先做数据库投影，再用数据库发布覆盖同名条目：`POST /api/editor/v1/lyrics/publish` 让一首歌立刻出现在 `/files/translation/lyrics/index.json` 与 `music_<id>.json`（含 `v2/{locale}/` 镜像），不需要改文件或重打镜像；`POST /api/editor/v1/lyrics/unpublish` 即使该曲存在于内嵌包内也会把它从索引和详情路由撤下——撤下标记写在 schema v35 的 `song_lyrics_public_withdrawals` 表里，与删除 publication 行同一个事务，发布时清除，并随内容备份一起携带。内嵌包加载失败不阻塞重建，投影只用数据库内容并把状态记为 degraded。

**什么时候真正落到公开路径。** 发布/取消发布、整首文档发布、source-v3 保存（含控制台的协作保存）、译本元数据变更、`music`/`title` 批量修改都会请求一次立即重建（`PublishNow`），其余写入走去抖重建。`GET /api/projection/status` 返回最近发布的 `generation`、是否 `pending`、`lastSuccessAt` 与脱敏 `lastError`；数据库在批量写返回时就已经落库，而该次保存对应的 `/files` 与 `/translation` 字节要等状态推进到更晚的非 pending generation 且 `lastError` 为空之后才算生效。控制台侧栏和管理面板的「立即全量发布」按钮调用 `POST /api/projection/publish`，触发一次全量构建并回传同一个状态对象。

## 卡牌剧情与区域对话

卡牌剧情（`kind=card`，故事 ID 是卡牌 ID，有前篇 `1` 和后篇 `2`）与区域对话（`kind=area`，故事 ID 是 scenarioId，只有 `1`）翻译成 `zh-CN` 与 `en-US`，日文脚本只读。数据在 schema v39 的 `side_stories`、`side_story_episodes`、`side_story_lines`、`side_story_line_localizations` 四张表里，行以去掉首尾空白的日文原文为键，来源为 `official`、`llm`、`human`。

**路由。** 编辑可用 `GET /api/editor/v1/stories`（列表）、`GET /api/editor/v1/story/{kind}/{id}`（详情）、`GET /api/editor/v1/story/{kind}/{id}/{episode}/snapshot`（TXT 导入快照）、`GET /api/editor/v1/stories/sync`（回填状态）和 `PUT /api/editor/v1/story/{kind}/{id}/{episode}`。PUT 走与其他内容写入相同的门禁，每行带 `expectedRevision`，整批全有或全无：未知行返回 `422 unknown_lines`，修订冲突返回 `409 revision_conflict`。管理员另有 `POST /api/editor/v1/story/{kind}/{id}/ai`（producer 任务 `ai-side-story`，只填空行）、`POST /api/editor/v1/story/{kind}/{id}/refresh`（立即重抓）和 `POST /api/editor/v1/stories/sync`（立即跑一轮回填）。agent 用法和每条路由的示例见 [`contracts/editor-api/README.md`](contracts/editor-api/README.md) 第 8 节，合同见 [`PRODUCTION_CONTRACT.md`](PRODUCTION_CONTRACT.md) 的 Card Stories And Area Talk。

**后台回填。** 设置 `side_story_backfill.enabled` 不为 false（未设置即为开，管理设置里可随时暂停）且 `SIDE_STORY_BACKFILL_ENABLED` 不为 false 时，服务进程按轮抓取日文脚本和官方 CN/EN 脚本。它按 TalkData 下标配对写入官方译文，覆盖文本不同的 `official` 行、接管所有 `llm` 行，从不改动 `human` 行，也从不调用 LLM。请求间隔只约束后台回填，管理员的重抓和 TXT 导入快照不受它限制。已列出资源路径的官方脚本返回 404 或返回的仍是日文时，该语言保持 `pending`，24 小时后重试；TalkData 条数与日文不同时标为 `mismatch`（脚本内的 ScenarioId 只是标签，不做比较）；`absent` 只表示该服务器没有这一话的资源路径。目录每 6 小时刷新一次，也可以在回填面板点「刷新目录」；按上游数据版本刷新要靠只在 `scheduler.enabled` 开启时运行的 upstream watcher，生产上不会发生。内容备份恢复后，回填不等 6 小时就重建目录。以下 env 每次启动都在打开数据库之前校验，非法值会让启动失败，数据库不会先被迁移（它们和下面 6 个上游 env 都列在 `.env.example` 里）：

| env | 默认 | 取值 |
| --- | --- | --- |
| `SIDE_STORY_BACKFILL_ENABLED` | `true`（未设置或为空） | `strconv.ParseBool` 接受的值 |
| `SIDE_STORY_BACKFILL_INTERVAL_MS` | `60000` | 1000–86400000 |
| `SIDE_STORY_BACKFILL_BATCH` | `30` | 1–500 的规范整数 |
| `SIDE_STORY_BACKFILL_REQUEST_DELAY_MS` | `1000` | 100–60000 |

**上游。** 新增 6 个设置，和其他 `upstream.*` 设置一样只在首次启动由 env 写入，之后在管理设置里修改；留空时用默认值。这 6 个 env 每次启动、打开数据库之前还会按上游地址规则校验，不安全的地址会让启动失败并报出变量名：

| 设置 | env | 默认 |
| --- | --- | --- |
| `upstream.en_masterdata_url` | `UPSTREAM_EN_MASTERDATA_URL` | `https://metadata.pjsk.moe/en/master` |
| `upstream.en_masterdata_fallback_url` | `UPSTREAM_EN_MASTERDATA_FALLBACK_URL` | `https://raw.githubusercontent.com/Team-Haruki/haruki-sekai-en-master/main/master` |
| `upstream.jp_scripts_url` | `UPSTREAM_JP_SCRIPTS_URL` | `https://storage.exmeaning.com/sekai-jp-assets` |
| `upstream.jp_scripts_fallback_url` | `UPSTREAM_JP_SCRIPTS_FALLBACK_URL` | `https://assets.unipjsk.com/startapp`（只用于卡牌剧情） |
| `upstream.cn_scripts_url` | `UPSTREAM_CN_SCRIPTS_URL` | `https://sekai-assets-bdf29c81.seiunx.net/cn-assets/startapp` |
| `upstream.en_scripts_url` | `UPSTREAM_EN_SCRIPTS_URL` | `https://storage.exmeaning.com/sekai-en-assets` |

JP 与 CN 的目录沿用现有的 `upstream.jp_masterdata_url`、`upstream.cn_masterdata_url` 链。

**公开文件。** `zh-CN` 发布在 `/files/translation/cardStory/card_<cardId>.json` 与 `/files/translation/areaTalk/group_<n>.json`，`en-US` 发布在 `/files/v2/en-US/translation/` 下的同名路径；`n` 是 JP actionSet ID 整除 100，没有其他语言的镜像。PUT、refresh 和写入了行的 AI（中途失败时也发布已保存的批次）在响应前重建所涉故事的文件，正在进行的全量重建不会把这次发布或撤下换回去；回填每轮结束后逐个重建本轮写过的故事，只有目录变化时才走去抖的全量重建，窗口为 `FILES_REBUILD_DEBOUNCE_MS`（默认 300000 ms），持续有写入时最多等两个窗口。

**备份。** 内容备份的 `translation-content/manifest.json` 升到 `schemaVersion` 2，新增 `translation-content/side-stories.json`，与其他内容在同一个恢复事务里恢复。`schemaVersion` 1 的旧备份仍可恢复，但会清空卡牌剧情与区域对话的四张表；早于 v39 的程序拒绝恢复 `schemaVersion` 2 的备份。Git 目标每次只推一个 `backup.tar.gz`（设置 `MOESEKAI_BACKUP_ENCRYPTION_KEY` 时为 `backup.enc`），GitHub 拒收超过 100 MiB 的文件，所以它必须低于这个上限，体积估算见 [`PRODUCTION_CONTRACT.md`](PRODUCTION_CONTRACT.md) 的 Backup And Restore。回滚与恢复步骤见 [`ROLLBACK_RUNBOOK.md`](ROLLBACK_RUNBOOK.md)。
