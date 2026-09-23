# NextTrans

Project SEKAI 翻译校对系统。生产发布合同是 NEXT 自有的 standalone 单镜像：SQLite 是唯一编辑真源，一个 Go 进程同时提供控制台静态页、`/api`、`/sse`、`/yjs` 协作 WebSocket，以及 CDN 友好的 `/files/*` 与 `/translation/*` 公开文件（与旧系统格式完全兼容，pjsk.moe 侧零改动）。翻译词条、活动剧情、歌词草稿与发布状态都在同一个数据库里；公开 JSON 由数据库投影再生成，而不是手工维护的文件。当前数据库 schema 版本为 v36。

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

**写入门禁。** 9 条无版本号写路由（`PUT /api/entry`、`PUT /api/category/batch`、`PUT /api/lyrics/save`、`POST /api/lyrics/translation-editions`、`POST /api/lyrics/publish`、`POST /api/lyrics/unpublish`、`PUT /api/event-story/update`、`POST /api/event-story/promote-human`、`POST /api/backup/push`）已删除并返回 JSON 404；全部写入走 `/api/editor/v1/*` 严格路由。每次严格写必须携带与所编辑文档同一份门禁状态的 `X-Moe-Loaded-Producer-State: <instanceId>:<revision>:<completedGeneration>`：缺少该头返回 `428`，重复或畸形返回 `400`，producer 正在运行、revision/generation 过期或进程重启导致 instance 不匹配返回 `409` 并回带当前 producer 状态。唯一保留的宽松门禁写路由是 `POST /api/admin/lyrics-source-reviews/import`（SekaiText-Moe 仍在用），它已有严格双胞 `POST /api/editor/v1/admin/lyrics-source-reviews/import`。`/sse` 连接建立后的第一帧固定是 `gate.status`，因此新开的标签页无需额外轮询就知道当前能不能写。

**歌词公开投影。** 镜像内嵌的已验收 Public Lyrics v3 只读包只是冷启动基线（归档 SHA-256 与数量 pin 统一记在 `contracts/public-lyrics/baseline.json`）。每次 files-service 投影重建先做数据库投影，再用数据库发布覆盖同名条目：`POST /api/editor/v1/lyrics/publish` 让一首歌立刻出现在 `/files/translation/lyrics/index.json` 与 `music_<id>.json`（含 `v2/{locale}/` 镜像），不需要改文件或重打镜像；`POST /api/editor/v1/lyrics/unpublish` 即使该曲存在于内嵌包内也会把它从索引和详情路由撤下——撤下标记写在 schema v35 的 `song_lyrics_public_withdrawals` 表里，与删除 publication 行同一个事务，发布时清除，并随内容备份一起携带。内嵌包加载失败不阻塞重建，投影只用数据库内容并把状态记为 degraded。

**什么时候真正落到公开路径。** 发布/取消发布、译本元数据变更、`music`/`title` 批量修改都会请求一次立即重建（`PublishNow`），其余写入走去抖重建。`GET /api/projection/status` 返回最近发布的 `generation`、是否 `pending`、`lastSuccessAt` 与脱敏 `lastError`；数据库在批量写返回时就已经落库，而该次保存对应的 `/files` 与 `/translation` 字节要等状态推进到更晚的非 pending generation 且 `lastError` 为空之后才算生效。控制台侧栏和管理面板的「立即全量发布」按钮调用 `POST /api/projection/publish`，触发一次全量构建并回传同一个状态对象。
