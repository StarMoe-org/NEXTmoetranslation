# 编辑写入 API（给提交译文的 agent / 脚本）

下文 `$BASE` 指翻译后台的根地址（例如 `https://<翻译后台域名>`，不带结尾 `/`）。所有请求与响应都是 UTF-8 JSON；请求体上限 8 MiB，未知字段、重复键、尾随第二个 JSON 值一律 `400 {"error":"invalid body"}`。

## 1. 登录与令牌

```bash
curl -sS -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"<账号>","password":"<密码>"}'
# 200 {"token":"<JWT>","username":"…","role":"editor|admin","expiresAt":<Unix 秒>}
```

之后每个请求带 `Authorization: Bearer <JWT>`。令牌默认 168 小时有效（`TOKEN_TTL_HOURS`）；账号改密码、改角色或被删除后旧令牌立即失效（`401`）。

- **限流**：登录尝试按「客户端 IP」与「账号名」两个键分别计数，任一键在 5 分钟窗口内满 10 次即返回 `429 {"error":"too many authentication attempts"}` 与 `Retry-After: 300`。成功的登录也计数；服务端只看 socket 对端地址、忽略转发头，部署在平台反向代理后面时所有调用方可能共用同一个 IP 计数。**登录一次、复用令牌**，只在 `401` 或临近 `expiresAt` 时重新登录。
- **脚本里永远不要调用 `POST /api/auth/refresh`**：它把该账号的 `token_version` 加一，于是该账号此前签发的**所有**令牌（包括浏览器控制台里正在用的会话、共用这个账号的其他 agent）立即失效，并断开该账号的 `/sse` 连接和它所在的 Yjs 协作房间（房间里的其他协作者也会被迫重连）。并发刷新只有一个成功，另一个得到 `401 session was revoked`。

## 2. 门禁头 `X-Moe-Loaded-Producer-State`（可选）

所有内容写入都走 `/api/editor/v1/*`。脚本**可以不带**这个头：不带时请求按宽松门禁处理——取得编辑准入并持有共享内容锁；如果 producer（CN 同步、AI 翻译、备份恢复等）正在运行，立即返回 `409 {"error":"producer is running; reload before saving"}`。

带头时值必须来自 `GET /api/editor-gate/status`（返回 `{version,instanceId,revision,generation,completedGeneration,running,lastRun}`），格式 `<instanceId>:<revision>:<completedGeneration>`，且只能有一个：

| 状态 | 含义 | 处理 |
| --- | --- | --- |
| `400 {"error":"invalid loaded producer state"}` | 头重复或格式错 | 修正或干脆不带 |
| `409` + 门禁状态对象 | producer 正在运行，或门禁 revision / completedGeneration / instanceId（进程重启）已变化 | 重新读取后重试 |
| `428 {"error":"loaded producer state required"}` | 该路由必须带头；只有控制台专用的 `POST /api/editor/v1/lyrics/{musicId}/collab-ticket` 与 `POST /api/editor/v1/backup/push` 两条 | agent 不应调用这些路由 |

**409 的重试**：不要原样重放。先重新读取（门禁状态、要改的词条或歌词当前版本），在新数据上重新生成请求，再提交；若门禁 `running=true`，每 10–30 秒轮询 `GET /api/editor-gate/status` 直到 `running=false`。建议最多重试 3 次并逐次加长间隔。

## 3. 写普通词条

```bash
curl -sS -X PUT "$BASE/api/editor/v1/entry" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"category":"cards","field":"prefix","key":"<日文原文键>","text":"<中文译文>","source":"human"}'
# 200 {"status":"ok"}，内容未变时 {"status":"noop"}
```

- 请求体：`{category, field, key, text, source, locale?, clientId?}`。`category` 取 `cards skills events information music gacha gachaInfo virtualLive sticker comic mysekai costumes characters units`；`source` 只接受 `cn human pinned llm unknown`。官方 CN 同步会覆盖除 `pinned` 以外的来源。
- `gachaInfo`（卡池简介与说明）的字段是 `summary`、`bubbleText`、`description`，取自 `gachas.json` 的 `gachaInformation`。`key` 是该字段的完整日文原文，换行和首尾空白都逐字保留（JSON 中换行写作 `\n`），不要 trim 或重新排版；先用 `GET /api/entries?category=gachaInfo&field=<字段>` 取回原键再提交。`text` 里的换行原样保存。长文只受 8 MiB 请求体上限限制。
- 词条必须已存在（先 `GET /api/entries?category=&field=` 取键）；不存在的键返回 `409 entry source identity changed; reload before saving`。
- 省略 `locale` 写中文；`locale` 只接受 `zh-CN`、`en-US`（`ja-JP` 只读，`400`）。加查询参数 `?response=correlated-v1` 可让成功响应回带 `category/field/key/text/source`。

## 4. 整首歌词文档：`/api/editor/v1/lyrics/document`

| 请求 | 权限 | 作用 |
| --- | --- | --- |
| `GET /api/editor/v1/lyrics/document?musicId=<id>` | 任何已登录用户。只读：不需要 `X-Moe-Loaded-Producer-State`，不取内容锁，producer 运行时照样返回 | 把一首歌导出成 `PUT` 的请求体 |
| `PUT /api/editor/v1/lyrics/document` | 仅管理员 | 一次提交整首歌，写成 source-v3 文档并**直接发布**：替换该曲原有歌词（含全部译本），并清除它的撤下标记。`dryRun: true` 执行同样的写入后回滚，并报告将要发生的改动 |
| `POST /api/editor/v1/lyrics/document/takeover` | 仅管理员 | 服务端把公开站当前提供的这首歌导出后原样发布：公开内容不变，之后可以按文档编辑（4.7） |
| `POST /api/editor/v1/lyrics/document/ruby` | 任何已登录用户。不读写任何歌曲，不需要门禁头，不取内容锁 | 给 `ja` markup 里还没有注音的汉字补上词典读音，供核对后写进 `document`（4.5.2） |

推荐流程：

1. `GET` 取出 `document`，按 4.1 的表处理 `warnings`。
2. 在 `document` 上改动（4.5 的配方），`expectedRevision` 保持导出的值。
3. 加上 `"dryRun": true` 提交 `PUT`，按 4.4 核对响应里的 `changes` 恰好是想要的改动。dryRun 执行与正式提交相同的写入再回滚，所以它通过了，正式提交在同一状态下也会通过。
4. 去掉 `dryRun` 再提交一次，`expectedRevision` 不变。收到 `409 revision_conflict`，说明读取之后有人改过这首歌（包括在控制台保存旧版草稿）：回到第 1 步重新读取，在新数据上重做改动。只把 `expectedRevision` 换成 `current.revision` 再提交，会覆盖别人的修改。
5. 按第 7 节确认公开文件已更新。

公开文件在发布后异步重建，刚发布完的一小段时间里公开站仍提供旧内容。`GET`、`PUT`、takeover 在读公开内容之前，会先等正在排队的重建完成（最多 10 秒），所以发布后马上重新导出，拿到的就是刚发布的内容，接着改下一处不会把上一次的修改改回去（`TestLyricsDocumentExportWaitsForAPendingPublication`）。等满 10 秒仍没重建完时照常返回，导出带 `served_revision_differs` 警告，这时按第 7 节等 `pending=false` 后重新读取。

第 3、4 步需要管理员令牌。

### 4.1 读取：`GET /api/editor/v1/lyrics/document?musicId=<id>[&from=served|database]`

```bash
curl -sS "$BASE/api/editor/v1/lyrics/document?musicId=<id>" -H "Authorization: Bearer $TOKEN"
```

- `from` 取 `served`（缺省）或 `database`，其他值返回 `400 invalid_query`，详情为 `from must be served or database`。
  - `served`：读公开站此刻实际提供的 `/files/translation/lyrics/music_<id>.json`，不论它来自内嵌基线包、旧版数据库发布、恢复导入台账还是 source-v3 投影。`version` 为 1、2、3、4 的详情都能读；v4 详情的每个译本都会导出（配方 12）。公开站没有提供这首歌时（例如已撤下）改读数据库，响应里的 `from` 为 `database`。
  - `database`：读数据库里可编辑的状态，即该曲的 source-v3 文档及其全部译本（撤下与否都读）；没有 source-v3 文档时读旧版草稿。用法见 4.6。
- 成功返回 `200`：`{"musicId": int, "from": "served"|"database", "servedVersion": int, "servedRevision": int, "document": {…}, "warnings": [{…}]}`。
  - `from` 是实际读取的来源。
  - `servedVersion`、`servedRevision` 是公开详情顶层的 `version` 与 `revision`；公开站没有提供这首歌时两者都为 `0`。
- `404 not_found`：公开站没有提供这首歌，数据库里也没有可编辑的歌词。响应体给出新建这首歌时要发送的 `expectedRevision`：

  ```json
  {"error": "not_found",
   "details": ["the database holds no editable lyrics for musicId 901; a PUT that creates them must send expectedRevision 0"],
   "current": {"revision": 0}}
  ```

  按 4.3 从头写请求，`expectedRevision` 填 `current.revision`：全新的歌是 `0`，其他情况是按 4.2 计算的当前值。GET 之后若数据库里出现了歌词（例如有人刚保存了旧版草稿），这次 `PUT` 会返回 `409`，不会悄悄替换掉它们。（`TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest` 断言 `current` 为 `{"revision":0}`。）
- `musicId` 不是正整数返回 `400 invalid_query`；未登录返回 `401`；服务端没有接上公开文件服务时返回 `503 projection_unavailable`（生产环境不会出现）。

`document` 可以直接作为 `PUT` 的请求体（不含 `dryRun`），写法如下：

- 注音写成 `{漢字|かな}`；日文里字面的 `{`、`}` 写成 `{{`、`}}`。
- 一行由不同演唱者分唱时带 `segments`，每段都写出自己的 `performerIds`（`[]` 表示这一段没有演唱者）。
  - 相邻两段各自最多一名演唱者、且演唱者相同时，导出合并成一段，因为主站把它们画成同一段颜色。
  - 有多名演唱者的段保留原来的边界，因为主站把每个多人段单独画成一段渐变。
  - 合并后只剩一段的行不带 `segments`。
- 不分段的行，行级 `performerIds` 只在与该 rendition 的默认演唱者不同时出现。显式的 `"performerIds": []` 表示这一行没有演唱者，不要删掉：删掉后会回落到默认演唱者。演唱者顺序与公开详情相同。
- 只有 Game 的 rendition 导出为 `"game": "only"`、`"lines": []` 加 `gameLines`。
- 某个 rendition 公开的署名与 `PUT` 默认会给它的署名不同时，这个 rendition 带自己的 `translationCredits`。
- 歌曲有多个译本时，`document` 带 `translationEditions`（默认译本在前）。各行的 `zh` 和署名属于默认译本；其他译本的译文在行的 `zhEditions` 里，署名在 rendition 的 `editionCredits` 里，写法见配方 12。只有隐式译本（key `main`，名称 `默认译本`）的歌不带这三个字段。
- `source.url` 取第一个带 Full 文本署名的 rendition 的修订链接；没有 Full 文本署名时取 Game 文本的。
- `expectedRevision` 已填成 `PUT` 比对的值（4.2），不要改。

`warnings` 总是数组，每项为 `{code, rendition?, side?, line?, message}`：`side` 为 `full` 或 `game`，`line` 是该面内从 0 开始的行号。`[]` 表示原样提交 `document` 后主站显示的内容不变；行 ID、`revision`、`updatedAt` 由 `PUT` 重新生成。

| `code` | 含义 | 提交前 |
| --- | --- | --- |
| `served_revision_differs` | 公开站提供的 revision 与数据库里应当公开的 revision（4.2 比较规则第 1 步）不同。原因有两种：① 公开投影还没重建完；② 数据库里那个 revision 的 source-v3 译文没有任何 rendition 带翻译或校对署名，投影永远不会提供它，这时警告会一直存在 | 按第 7 节等到 `pending=false` 后重新读取。警告仍在就是原因 ②：要保留数据库里的版本，用 `from=database` 读出、补上署名后提交；原样提交本文档，则用公开版本替换数据库里那个未署名的版本 |
| `unpublished_draft_replaced` | 数据库里有一份旧版草稿，内容与公开站提供的不同（与 revision 大小无关）；`expectedRevision` 已计入它的 revision（4.2 比较规则第 2 步） | 提交会删除这份草稿，草稿里的修改不在本文档里。要在草稿基础上改，改用 `from=database` 导出（4.6） |
| `withdrawn_republished` | 歌曲已撤下（只在读取数据库时出现）；提交这份文档会重新公开它 | 确实要重新公开才提交 |
| `credit_missing` | 没有任何 rendition 带翻译或校对署名；`PUT` 会以 `field: "translationCredit"` 的 issue 拒收 | 补 `translationCredit` / `proofreadingCredit`，或给 rendition 加 `translationCredits`（4.5 配方 9） |
| `source_missing` | 公开详情没有任何来源署名，`source.url` 为空 | 自己填 `source.url`，否则返回 `422 source_revision_required` |
| `source_unsupported` | 署名的来源链接 `PUT` 不接受：站点不在 4.3 的列表里，或没有唯一的 `oldid` | 换成受支持的修订链接，否则返回 `422 source_revision_required` |
| `source_url_reencoded` | `PUT` 会把来源链接规范化成另一种编码（`url.PathEscape`）。公开署名里链接的字面会变，指向的修订不变 | 无需处理 |
| `source_differs` | 该 rendition 的部分内容署名的是另一个修订；`PUT` 把整首歌归到 `source.url` 这一个修订 | 提交后这部分内容的公开署名改为 `source.url`，原来的修订不再出现。必须保留那个署名时，不要用本路由提交 |
| `rendition_inferred` | v1/v2 详情没有 rendition，按演唱者推断成 `sekai` 或 `vocaloid`，标签取缺省值 | 检查 `key`、`kind`、`label` |
| `game_projection_not_ordered` | Game 投影没有按顺序选取 Full 行（或引用了不存在的行），导出为 `independent` 的 `gameLines`；发布后 Game 面不再是 Full 的精确投影 | 改 `zh` 时 `lines` 与 `gameLines` 都要改 |
| `game_projection_differs` | Game 行与它投影的 Full 行在 ja、zh、en、段落、分段（含注音和演唱者）或尾随演唱者上不同，导出为 `independent`。发布后公开详情的 `relation` 不再是精确投影，各行内容不变 | 同上 |
| `side_label_differs` | Game 面的标签与 Full 不同；`PUT` 两面都用 rendition 的 `label`，主站只显示 rendition 的 `label` | 无需处理 |
| `trailing_performers_dropped` | 这一行的尾随演唱者无法携带 | 提交后公开详情里这一行没有尾随演唱者 |
| `unknown_performer` | 演唱者既不是游戏角色也不是已审计外部歌手，已从导出中去掉 | 提交后公开详情里这一行（或这一段）没有这个演唱者 |
| `ruby_not_served` | v1 详情没有注音，含汉字的行 `PUT` 会报 `field: "ja"` 的 issue | 为每个汉字补上注音（4.5 配方 2）；可以先用 4.5.2 取词典读音，再逐个核对 |
| `ruby_not_expressible` | 注音不是“汉字基字 + 假名读音”，注音标记表达不了，`PUT` 会拒收该行 | 修正该行注音 |
| `english_dropped` | `PUT` 不存英文行 | 提交后公开详情里这一行没有英文 |

`PUT` 一定会拒收的导出：`warnings` 含 `source_missing`、`source_unsupported`、`ruby_not_served`、`ruby_not_expressible` 或 `credit_missing`，或 `document` 里没有任何一行 `zh`（issue `at least one line needs a zh translation to publish`）。这些问题要先在 `document` 里修好。

内嵌基线包的往返结果：`TestLyricsDocumentExportRoundTripsTheEmbeddedBundle` 对全部 694 首做“导出 → `PUT` → 与公开详情比对 → 再导出”。

- 1 首完全相同。
- 642 首只在不影响主站显示的字段上不同：`revision`、`updatedAt`、行 ID（`PUT` 按位置重新命名）、`sourceTabPaths`、来源署名按组件的拆分方式、与注册表颜色相同的演唱者颜色、Game 面标签，以及两类边界——相邻、各自最多一名且相同演唱者的分段边界，相邻无注音片段的边界。
- 多名演唱者分段的边界全部保留：相邻两段是同一组多名演唱者的行共 11 行，分布在歌曲 68、299、307、378、637（测试日志 `map[68:1 299:4 307:2 378:1 637:3]`）。
- 50 首在上述字段之外还有差异，全部有对应警告：34 首是 `relation`（`game_projection_differs`），18 首是来源署名的 `revisionUrl`（`source_url_reencoded`），有的歌两项都有。
- 1 首（795）的来源在 `zh.moegirl.org.cn`，`PUT` 以 `422 source_revision_required` 拒收，导出时已给出 `source_unsupported`。
- 按警告计（一首可能有多个）：`game_projection_differs` 34 首、`source_url_reencoded` 18 首、`side_label_differs` 11 首、`source_unsupported` 1 首，其他警告在基线包里没有出现。
- 逐行比较了 48661 行：ja、zh、en、段落、分段文本、各段演唱者及其顺序、注音和尾随演唱者都没有差异。
- 其余 693 首原样提交导出时，`changes` 都是 `{"against":"served","changed":false}`。发布后再导出得到同一个请求（只有 `expectedRevision` 和规范化后的 `source.url` 会变），且没有警告。

### 4.2 `expectedRevision`：什么时候必填、填什么

- **必填**：只要这首歌在数据库里存有、或公开站正在提供任何歌词，`PUT` 就必须带 `expectedRevision`。“存有”包括旧版草稿或发布、source-v3 文档、恢复导入台账条目和内嵌基线包条目。缺少时返回 `422 expected_revision_required`，`current.revision` 就是应发送的值：

  ```json
  {"error": "expected_revision_required",
   "details": ["expectedRevision is required because the song already has lyrics stored or served; send 2, the revision GET reports, to replace them"],
   "current": {"revision": 2}}
  ```

  什么都没有的歌可以省略它，或发送 `0`（`TestPublishLyricsDocumentRequiresExpectedRevisionOnlyWhenSomethingIsStoredOrServed`）。
- **填什么**：GET 返回 `200` 时用 `document.expectedRevision`，返回 `404` 时用 `current.revision`。不要改用 `servedRevision` 或公开文件顶层的 `revision`。
- **比较规则**：`PUT` 要求 `expectedRevision` 等于当前值，当前值按下面的规则计算：
  1. 先取“公开 revision”，即以下三者中最大的：旧版发布的 revision、内嵌包的 revision、source-v3 译文的 revision（只在大于 1 时计入，1 是未编辑的恢复基线）。
  2. 再与旧版草稿的 revision 取较大者，因为发布会删除这份草稿。
  3. 歌曲存有或正在提供任何歌词时，当前值至少为 1，所以 `0` 只对应什么都没有的歌。

  不相符返回 `409 revision_conflict`，详情为 `expectedRevision N does not match the current revision M`，`current.revision` 是当前值（`TestPublishLyricsDocumentRevisionsStayAboveBundleAndCheckExpectedRevision`：内嵌包 revision 7 的歌，发送 3 得到 `current` `{"revision": 7}`）。
- **新 revision**：发布得到的 revision 比旧版草稿、旧版发布、source-v3 译文、译本状态和内嵌包的 revision 都大，且至少为 2。什么都没有的歌第一次发布得到 2（`TestPublishLyricsDocumentFullOnlyWithRubyIsServedByTheProjection`）；内嵌包 revision 7 的歌得到 8。

### 4.3 `PUT` 请求字段

- `musicId`（必填）：正整数，必须在目录里。
- `expectedRevision`：见 4.2。
- `source.url`（必填）：须满足以下全部条件，否则返回 `422 source_revision_required`：
  - 站点是 `vocaloid.fandom.com`、`projectsekai.fandom.com`、`www.sekaipedia.org` 或 `moegirl.icu`；
  - 路径是 `/wiki/<页面>` 或 `index.php?title=<页面>`（也接受 `/w/index.php`）；
  - 带唯一的 `?oldid=<正整数>`。

  页面名里的 `?` 写成 `%3F`。公开详情里记录的是规范化后的 `https://<站点>/wiki/<页面>?oldid=<N>`。`projectsekai.fandom.com` 与 `vocaloid.fandom.com` 同属 provider `vocaloid_fandom`（由数据库迁移 v37 放开）。`source.title` 可选，缺省取页面名。
- `translationCredit` / `proofreadingCredit`：文档级公开署名，每项单行、不超过 2048 字节。
  - 它们是默认译本的署名，给每个有 `zh`、且没有自己 `translationCredits` 的 rendition。
  - 存在这样的 rendition 时至少要填一个，否则得到 `field: "translationCredit"` 的 issue；每个带 `zh` 的 rendition 都自带署名时可以都不填（`TestPublishLyricsDocumentNeedsDocumentCreditsOnlyForRenditionsWithoutTheirOwn`）。
  - 无论哪种写法，最后至少要有一个 rendition 带署名，因为公开站不提供没有署名的歌。
- `translationEditions`（可选）：这首歌的全部 `zh-CN` 译本，`[{"key", "label"}]`，第一项是默认译本。写法见配方 12。
  - 默认译本的译文就是各行的 `zh`，署名就是文档级署名或 rendition 的 `translationCredits`。其他译本的译文写在行的 `zhEditions` 里，署名写在 rendition 的 `editionCredits` 里。
  - 省略或写 `[]` 表示只有隐式译本 `main`（名称 `默认译本`），与 `[{"key": "main", "label": "默认译本"}]` 等价。
  - 带了就要有 1–16 项：`key` 匹配 `^[a-z0-9][a-z0-9._-]{0,127}$`、不重复，且其中必须有 `main`；`label` 去掉首尾空白后为 1–256 字节。
  - “至少一行 `zh`、至少一个署名”只要求默认译本。其他译本可以是空白的，控制台新建的译本就是空白的。
- `renditions[]`（1–16 个），每个 rendition 的字段：
  - `key`：匹配 `^[a-z0-9][a-z0-9._-]*$`，不重复。
  - `kind?`：`original|sekai|vocaloid|alternate`，缺省等于 `key`。
  - `label?`：缺省按 kind 取 `Original Version` / `SEKAI Version` / `VIRTUAL SINGER Version`；`alternate` 必须自带。
  - 其余为 `performerIds?`、`game?`（缺省 `none`）、`lines[]`、`gameLines?`、`translationCredits?`、`editionCredits?`。
- 每行的字段：
  - `ja`（必填）：单行，不超过 8192 字节。
  - `zh?`：默认译本的译文，单行，不超过 16384 字节。全曲至少要有一行 `zh`。
  - `zhEditions?`：其他译本里这一行的译文，`{"<译本 key>": "…"}`，每项的规则与 `zh` 相同。缺少某个 key，表示那个译本的这一行为空。
  - `performerIds?`、`segments?`、`stanzaBreakBefore?`、`inGame?`。
  - source-v3 文档不存英文，`en` 非空会得到 `field: "en"` 的 issue（`English lines are not stored for source-v3 documents; remove en`），请省略。
- 注音：`ja` 中**每个汉字**都必须写在 `{漢字|かな}` 里，写法见 4.5 配方 2。字面的 `{`、`}` 写成 `{{`、`}}`；花括号外的 `|` 是普通字符。`zh` 是纯文本，原样保存，不需要转义。
- `performerIds`：游戏角色 ID 1–26（`GET /api/catalog/characters`）或已审计外部歌手的数字 ID。
  - 行级值覆盖 rendition 默认值。rendition 缺省为目录里对应 kind 的演唱角色，`alternate` 没有缺省。
  - 按给出的顺序保存，主站按这个顺序画颜色渐变和头像；重复的 ID 只保留第一次出现的。
  - 公开详情里 rendition 的演唱者名单 `performers` 仍按 ID 排序。
- `segments`（可选，1–100 段）：写法见 4.5 配方 5。
- `game`：取值如下，写法见 4.5 配方 6。
  - `none`：只有 Full。
  - `same`：Game 与 Full 相同。
  - `cut`：Game 恰好是 `inGame: true` 的 Full 行。
  - `independent`：Game 用自己的 `gameLines`。
  - `only`：只有 Game。

  全曲的 rendition 都是 `only` 时公开状态为 `game_only`，否则为 `complete`。
- `translationCredits`（可选，rendition 自己的署名）：`{"translation"?, "proofreading"?}`，每项单行、不超过 2048 字节。带了就替换这个 rendition 的署名，不论它有没有 `zh`；`{}` 表示这个 rendition 不署名。
- `editionCredits`（可选）：这个 rendition 在其他译本里的署名，`{"<译本 key>": {"translation"?, "proofreading"?}}`。每项单行、不超过 2048 字节，首尾空白会去掉；缺少某个 key，表示那个译本里这个 rendition 不署名。
- `zhEditions` 与 `editionCredits` 的 key 必须是 `translationEditions` 里声明的非默认译本，没有 `translationEditions` 时不能带这两个字段。`zhEditions` 可以写在任何 rendition 的 `lines` 上，以及 `independent`、`only` 的 `gameLines` 上；`same`、`cut` 的 Game 没有自己的行，跟随 Full 行。
- `dryRun: true`：执行全部校验，在事务里执行与正式提交完全相同的写入（含 4.8 的接管记录），构造将要公开的 `document` 并计算 `changes`，然后**回滚**。
  - 什么都不留下，不重建公开文件，也不重置协作房间。
  - 正式提交会遇到的错误，包括只有数据库才会报的错误，dryRun 同样返回（`TestPublishLyricsDocumentDryRunFailsWhereThePublishFails`）。

成功返回 `200`：`{"dryRun": bool, "musicId": int, "revision": int, "publicPath": "/files/translation/lyrics/music_<id>.json", "document": <将被公开的详情 JSON>, "changes": {…}}`。
- `revision` 是这次发布（或 dryRun 预演）将得到的新 revision，不是当前值。
- `document` 是公开站将要提供的详情：歌曲只有一个译本时是 v3（`contracts/public-lyrics/v3/detail.schema.json`），有多个译本时是 v4（`contracts/public-lyrics/v4/detail.schema.json`；`TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail` 断言它与公开文件逐字节相同）。
- `changes` 的读法见 4.4。

GET 返回 `404` 的歌，按下一节的基准文档从头写请求，`expectedRevision` 填 404 响应的 `current.revision`。

### 4.4 用 `changes` 确认改动

`PUT` 与 takeover 的响应都带 `changes`，它比较这次提交产生的详情与这首歌的现状。字段 `against` 指明比较对象：`served` 是公开站当前提供的内容；`database` 是数据库里可编辑的状态，在公开站没有提供时使用（例如已撤下）；`nothing` 表示这首歌什么都没有，所有 rendition 都列为新增。两侧都先转换成请求格式再比较，所以以下内容不算改动：

- `PUT` 会重新生成的字段：行 ID、`sourceTabPaths`、来源署名的组件元数据、`revision`、`updatedAt`；
- 渲染结果相同的边界；
- GET 已用警告报告过、请求格式装不下的内容。

原样提交导出的 `document`，得到的正好是 `{"against":"served","changed":false}`（`TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest`、`TestLyricsDocumentChangesAreEmptyForAnUnchangedExport`）。有多个译本的歌也一样（`TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail`、`TestPublishLyricsDocumentKeepsTheTranslationEditionsOfSong682`）。

结构（空的项省略）：

```text
{against, changed, truncated?,
 source?: {before: {url, title} | null, after: {url, title}},
 translationEditions?: {before: [{key, label}], after: [{key, label}]},
 renditionsAdded?, renditionsRemoved?: [{key, game, lines, gameLines}],
 renditions?: [{key,
                kind?, label?, game?: {before, after},
                translationCredits?: {before: {translation?, proofreading?}, after: {…}},
                editionCredits?: {"<译本 key>": {before: {translation?, proofreading?}, after: {…}}},
                sides?: [{side: "full"|"game", linesBefore, linesAfter, addedCount, removedCount, changedCount,
                          added?: [行摘要], removed?: [行摘要],
                          changed?: [{before, after, fields, ja?: {before, after}, zh?: {before, after},
                                      zhEditions?: {"<译本 key>": {before, after}},
                                      segments?: {before: [{ja, performerIds}], after: [{ja, performerIds}]}}]}]}]}

行摘要 = {line, ja, zh?, stanzaBreakBefore?, performerIds, segments?: [{ja, performerIds}], inGame?, zhEditions?: {"<译本 key>": "…"}}
```

- `renditions[]` 只列两侧都有、且有改动的 rendition。`translationCredits` 比较的是这个 rendition 在默认译本里实际公开的署名，不论署名来自文档还是 rendition 自己。`editionCredits` 按 key 比较其他译本的署名，只列变了的译本，`{}` 表示不署名。
- `translationEditions`：译本列表或其顺序（即默认译本）变了时出现，前后都是完整列表，默认译本在前。不带 `translationEditions` 的一侧按 `[{"key":"main","label":"默认译本"}]` 计；`against: "nothing"` 的新歌只在请求声明了译本时出现，`before` 为 `[]`。
- `sides[]`：`full` 对应 `lines`，`game` 对应 `gameLines`。`same`、`cut` 的 Game 没有自己的行，它的改动体现为 `game` 与 `inGame`。
- 行号：`added[].line` 是提交后的位置，`removed[].line` 是提交前的位置，`changed[]` 同时带 `before` 和 `after`，都从 0 开始。
- 配对规则（`lyricsDocumentAlignLines`），每个 side 分别计算：
  1. 两侧完全相同的行先按最长公共子序列对齐，作为锚点，不列出。“完全相同”指日文 markup、解析后的分段与演唱者、`zh`、`zhEditions`、段落空行和 `inGame` 全部相同。
  2. 相邻两个锚点之间，可见日文相同、或 `zh` 相同且非空的行，再按最长公共子序列配成 changed。
  3. 剩下的行在相邻两个已配对的行之间按位置一一配成 changed；只在一侧多出来的行才算 added 或 removed，排在这一段的末尾。

  所以整行换成完全不同的内容，报的仍是一条 changed，`fields` 为 `["ja","ruby","zh"]`。拆行、合行、插入或删除一行时，只有多出或少掉的那一行算增删，其后的行不受影响。按位置配成的 changed 不一定是你心里的对应关系，核对时要看 `before` / `after` 的内容，不能只看行号。
- 行摘要（`added[]`、`removed[]` 的每一项）写出整行结构：
  - `ja`（markup）和 `zh`；
  - `stanzaBreakBefore`，只在为 `true` 时出现；
  - `performerIds`：解析后的整行演唱者（行级值，否则 rendition 的默认值），总是出现，`[]` 表示没有演唱者；
  - `segments`：解析后的分段，已填好继承来的演唱者，只在多于一段、或唯一一段的演唱者与整行不同时出现；
  - `inGame`：只在 `cut` rendition 的 Full 行上出现，值为 `true` 或 `false`；
  - `zhEditions`：这一行在其他译本里的非空译文。

  两行内容完全相同的相邻行之间挪动段落空行时（歌曲 328、502 就有这样的两行），配对结果是一条 removed 加一条 added，两条的 `ja`、`zh` 相同，靠 `stanzaBreakBefore` 和行号看出空行怎样挪动，读法见配方 4（`TestLyricsDocumentChangeSummariesShowTheStructureOfAddedAndRemovedLines`）。
- `fields` 的取值：
  - `ja`：可见的日文文本变了；
  - `ruby`：读音或注音的拆分变了；
  - `segments`：分段边界变了；
  - `performers`：演唱者变了；
  - `zh`：译文（默认译本）变了；
  - `zhEditions`：其他译本的译文变了；
  - `stanzaBreakBefore`：段落空行变了；
  - `inGame`：是否进 Game 变了。

  `ja` 变了或 `ruby` 变了时带 `ja` 的前后 markup；`zh` 变了时带 `zh`；`zhEditions` 变了时带 `zhEditions`，只列变了的译本，空串表示那个译本的这一行为空；边界或演唱者变了时带 `segments`，其中是解析后的分段，已填好继承来的演唱者。
- 计数总是完整的；全文档最多逐条列出 200 条，超出时 `truncated: true`（`TestLyricsDocumentChangesListABoundedNumberOfLines`）。

核对步骤：

1. 列出这次想改的项，例如“sekai 第 1 行只改 `zh`”。
2. 要求 `changed: true`，且 `renditions[]` 只含改动过的 rendition；每个 side 的 `addedCount` / `removedCount` / `changedCount` 与预期相同；每条 `fields` 都在预期之内；前后文本逐字核对。
3. 没改来源就不应出现 `source`，没改署名就不应出现 `translationCredits`，没动其他译本就不应出现 `translationEditions`、`editionCredits` 和 `zhEditions`。
4. 多出来的项通常说明整行替换时漏抄了字段：
   - 删掉了 `"performerIds": []`，会多出 `performers`；
   - 漏了 `inGame`，会多出 `inGame`；
   - 漏了 `stanzaBreakBefore`，会多出 `stanzaBreakBefore`；
   - 重打了日文，会多出 `ja` / `ruby`；
   - 漏了 `zhEditions`，会多出 `zhEditions`，其 `after` 为空串。

   这时修正请求，再 dryRun 一次。

### 4.5 编辑配方

配方 1–11 都在同一份基准文档上改动，配方 12 在它上面加了一个译本；每个配方给出 dryRun 得到的 `changes`。

基准文档：合成歌曲 901（测试目录里的 ID）按 `lyricsDocumentExportTestRequest` 发布后，GET 的原样输出：

```json
{"musicId": 901, "from": "served", "servedVersion": 3, "servedRevision": 2, "warnings": [], "document": {
  "musicId": 901,
  "expectedRevision": 2,
  "source": {"url": "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242", "title": "合成試験曲"},
  "translationCredit": "合成译者",
  "proofreadingCredit": "合成校对",
  "renditions": [
    {"key": "sekai", "kind": "sekai", "label": "SEKAI Version", "game": "cut", "performerIds": [1, 2], "lines": [
      {"ja": "{試験|しけん}の{歌|うた}", "zh": "测试之歌", "inGame": true},
      {"ja": "らららと{合成|ごうせい}", "zh": "啦啦啦地合成", "stanzaBreakBefore": true, "performerIds": [1]},
      {"ja": "{点線|てんせん}をなぞる", "zh": "描过虚线", "inGame": true, "performerIds": []}
    ]},
    {"key": "vocaloid", "kind": "vocaloid", "label": "VIRTUAL SINGER Version", "game": "independent", "performerIds": [21], "lines": [
      {"ja": "ミクのうた", "zh": "未来之歌"},
      {"ja": "ルルル", "zh": "噜噜噜"}
    ], "gameLines": [
      {"ja": "ミクのうた", "zh": "未来之歌", "stanzaBreakBefore": true}
    ]}
  ]
}}
```

`TestExportLyricsDocumentReproducesAPublishedDocumentRequest` 证明这份导出等于发布时的请求。

约定：

- “把 `sekai` 第 2 行换成 X”指把 `document.renditions[0].lines[2]` 整个替换成 X，行号从 0 开始。
- 配方只写出替换的部分，其余保持基准文档原样。
- 提交时加上 `"dryRun": true`。
- 下文的 `changes` 用 `…` 省略与前一个示例相同的外层：`{"against":"served","changed":true,"renditions":[{"key":"sekai","sides":[{"side":"full","linesBefore":3,"linesAfter":3,"addedCount":0,"removedCount":0,"changedCount":1,"changed":[` 与 `]}]}]}`。

**配方 1：改日文错字。** 把 `sekai` 第 2 行的 `なぞる` 改成 `なぞって`，其余字段照抄：

```json
{"ja": "{点線|てんせん}をなぞって", "zh": "描过虚线", "inGame": true, "performerIds": []}
```

```json
{"against":"served","changed":true,"renditions":[{"key":"sekai","sides":[{"side":"full","linesBefore":3,"linesAfter":3,"addedCount":0,"removedCount":0,"changedCount":1,"changed":[{"before":2,"after":2,"fields":["ja"],"ja":{"before":"{点線|てんせん}をなぞる","after":"{点線|てんせん}をなぞって"}}]}]}]}
```

改的是汉字时读音要一起改。例如 `sekai` 第 0 行改成 `{"ja": "{試練|しれん}の{歌|うた}", "zh": "测试之歌", "inGame": true}`，得到 `…{"before":0,"after":0,"fields":["ja","ruby"],"ja":{"before":"{試験|しけん}の{歌|うた}","after":"{試練|しれん}の{歌|うた}"}}…`。

**配方 2：改注音。** 花括号里是 `{基字|读音}`。

- 只改读音：`sekai` 第 0 行 `{歌|うた}` → `{歌|ウタ}`（读音可以是片假名，见 `TestParseLyricsDocumentRuby`），`fields` 只有 `ruby`：

  ```json
  {"ja": "{試験|しけん}の{歌|ウタ}", "zh": "测试之歌", "inGame": true}
  ```

  `…{"before":0,"after":0,"fields":["ruby"],"ja":{"before":"{試験|しけん}の{歌|うた}","after":"{試験|しけん}の{歌|ウタ}"}}…`
- 改拆分：`{試験|しけん}` 给整个词一个注音，`{試|し}{験|けん}` 给每个字各一个注音；可见文本相同，所以 `fields` 只有 `ruby`（`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter` 断言的正是这一行）：

  ```json
  {"ja": "{試|し}{験|けん}の{歌|うた}", "zh": "测试之歌", "inGame": true}
  ```

  `…{"before":0,"after":0,"fields":["ruby"],"ja":{"before":"{試験|しけん}の{歌|うた}","after":"{試|し}{験|けん}の{歌|うた}"}}…`
- 熟字训和当て字整词标注：`{今日|きょう}` 是一个注音盖住两个汉字，读音不能拆到单字上。`{今|いま}{日|ひ}` 是两个独立注音，读作「いま」+「ひ」，意思不同。两种写法都合法，可见文本都是「今日」，所以两者互换时 `changes` 只报 `ruby`（`…"fields":["ruby"],"ja":{"before":"{今日|きょう}も{合成|ごうせい}","after":"{今|いま}{日|ひ}も{合成|ごうせい}"}…`）。每个字各有读音时才逐字拆开，如 `{試|し}{験|けん}`。
- 送假名写在花括号外：`{短|みじか}い`（`TestPublishLyricsDocumentGameModes` 用的就是 `{短|みじか}い{試験|しけん}`）。基字只能是汉字，`々` 也算汉字：`{時々|ときどき}` 合法。读音只能是假名，且以假名开头。
- 每个汉字都必须在某个 `{…|…}` 里。整行都没有注音时（例如 `ruby_not_served` 的行），可以先用 4.5.2 取词典读音，核对后再写进去。

下列写法会被拒收（箭头左边是 `ja`，右边是 `message`；示例都写在 `sekai` 的某一行），issue 形如 `{"rendition":"sekai","side":"full","line":0,"field":"ja","message":"…"}`：

- `{短い|みじかい}{合成|ごうせい}` → `ruby base "短い" in {短い|みじかい} at character 0 must contain only kanji`
- `{試験の|しけんの}{歌|うた}` → `ruby base "試験の" in {試験の|しけんの} at character 0 must contain only kanji`
- `{試|し}験の歌` → `kanji without a {kanji|reading}: 「験」 at character 5, 「歌」 at character 7`（`TestParseLyricsDocumentRuby`）
- `{時|とき}々なぞる` → `kanji without a {kanji|reading}: 「々」 at character 6`
- `{試験|shiken}の{歌|うた}` → `ruby reading "shiken" in {試験|shiken} at character 0 must be kana matching ^[ぁ-ゖァ-ヺー・゙゚]+$ and start with kana`
- `{試験}の{歌|うた}` → `ruby {試験} at character 0 must be written {kanji|reading}`

`character N` 是 `ja` markup 字符串里的字符下标，从 0 开始，花括号和读音都计入；分段里的问题也按整行 `ja` 计数。

**配方 3：拆行与合行。**

- 拆行：把 `sekai` 第 0 行换成两行，`zh` 也要拆开。`cut` 的 Game 里，两行各自要写 `inGame`。

  ```json
  [{"ja": "{試験|しけん}の", "zh": "测试的", "inGame": true}, {"ja": "{歌|うた}", "zh": "歌", "inGame": true}]
  ```

  ```json
  {"against":"served","changed":true,"renditions":[{"key":"sekai","sides":[{"side":"full","linesBefore":3,"linesAfter":4,"addedCount":1,"removedCount":0,"changedCount":1,"added":[{"line":1,"ja":"{歌|うた}","zh":"歌","performerIds":[1,2],"inGame":true}],"changed":[{"before":0,"after":0,"fields":["ja","ruby","zh"],"ja":{"before":"{試験|しけん}の{歌|うた}","after":"{試験|しけん}の"},"zh":{"before":"测试之歌","after":"测试的"}}]}]}]}
  ```
- 合行：把 `vocaloid` 的第 0、1 行换成一行。`independent` 的 `gameLines` 不受影响，要改得另外改。

  ```json
  [{"ja": "ミクのうた　ルルル", "zh": "未来之歌 噜噜噜"}]
  ```

  ```json
  {"against":"served","changed":true,"renditions":[{"key":"vocaloid","sides":[{"side":"full","linesBefore":2,"linesAfter":1,"addedCount":0,"removedCount":1,"changedCount":1,"removed":[{"line":1,"ja":"ルルル","zh":"噜噜噜","performerIds":[21]}],"changed":[{"before":0,"after":0,"fields":["ja","zh"],"ja":{"before":"ミクのうた","after":"ミクのうた　ルルル"},"zh":{"before":"未来之歌","after":"未来之歌 噜噜噜"}}]}]}]}
  ```

  在末尾追加一行时，`added` 列出新行，例如 `[{"line":3,"ja":"{新|あら}たな{歌|うた}","zh":"新的歌","performerIds":[1,2],"inGame":false}]`：新行没写 `inGame`，所以不进 `cut` 的 Game（`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter`）。

**配方 4：段落空行。** `stanzaBreakBefore: true` 表示这一行之前空一行。

- 在 `sekai` 第 2 行前加空行：`{"ja": "{点線|てんせん}をなぞる", "zh": "描过虚线", "inGame": true, "performerIds": [], "stanzaBreakBefore": true}`，得到 `…{"before":2,"after":2,"fields":["stanzaBreakBefore"]}…`。
- 去掉第 1 行前的空行：省略该字段，`{"ja": "らららと{合成|ごうせい}", "zh": "啦啦啦地合成", "performerIds": [1]}`，得到 `…{"before":1,"after":1,"fields":["stanzaBreakBefore"]}…`。
- 在两行内容完全相同的相邻行之间挪动空行：`changes` 报一条 `removed` 加一条 `added`，不报 `stanzaBreakBefore`。两条摘要除 `line` 外完全相同，`removed` 的 `line` 为 n，`added` 的 `line` 为 n+1：
  - 空行从第 n 行前挪到第 n+1 行前：两条都带 `"stanzaBreakBefore":true`，例如 `"removed":[{"line":1,"ja":"{繰|く}り{返|かえ}す","zh":"重复","stanzaBreakBefore":true,"performerIds":[1,2]}]`、`"added":[{"line":2,…同上}]`（`TestLyricsDocumentChangeSummariesShowTheStructureOfAddedAndRemovedLines`）。
  - 空行从第 n+1 行前挪到第 n 行前：两条都不带 `stanzaBreakBefore`，表示不带空行的那一行从第 n 行挪到了第 n+1 行。

**配方 5：分段演唱者。** `segments` 把一行拆成由不同演唱者唱的几段。

- 各段 `ja` 按顺序拼接后必须与这一行的 `ja` 逐字相同，注音标记也算在内。
- 每段单独解析，所以注音不能跨段，段也不能为空或只有空白。
- 段的 `performerIds` 省略或为 `null` 时，用这一行的演唱者；这一行也没写时，用 rendition 的演唱者。`[]` 表示这一段没有演唱者。
- `zh` 仍按整行写。

`sekai` 第 0 行前半由角色 1 唱、后半由角色 2 唱：

```json
{"ja": "{試験|しけん}の{歌|うた}", "zh": "测试之歌", "inGame": true, "segments": [
  {"ja": "{試験|しけん}の", "performerIds": [1]},
  {"ja": "{歌|うた}", "performerIds": [2]}
]}
```

`…{"before":0,"after":0,"fields":["segments"],"segments":{"before":[{"ja":"{試験|しけん}の{歌|うた}","performerIds":[1,2]}],"after":[{"ja":"{試験|しけん}の","performerIds":[1]},{"ja":"{歌|うた}","performerIds":[2]}]}}…`

- 继承：`{"ja": "{試験|しけん}の{歌|うた}", "zh": "测试之歌", "inGame": true, "performerIds": [2], "segments": [{"ja": "{試験|しけん}の"}, {"ja": "{歌|うた}", "performerIds": [1, 2]}]}` 解析后第一段是 `[2]`，changes 的 `after` 为 `[{"ja":"{試験|しけん}の","performerIds":[2]},{"ja":"{歌|うた}","performerIds":[1,2]}]`。演唱者按给出的顺序保存，`[2, 1]` 与 `[1, 2]` 不同（`TestLyricsDocumentSegmentsCarryPerSegmentPerformersInTheirOrder`）。
- 只改整行演唱者、不分段：`{"ja": "{試験|しけん}の{歌|うた}", "zh": "测试之歌", "inGame": true, "performerIds": [2]}` 得到 `…"fields":["performers"],"segments":{"before":[{"ja":"{試験|しけん}の{歌|うた}","performerIds":[1,2]}],"after":[{"ja":"{試験|しけん}の{歌|うた}","performerIds":[2]}]}…`。
- 分段和译文可以同时改，`fields` 为 `["segments","zh"]`（`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter`）。

分段问题报为 `field: "segments"` 的 issue，逐段的问题以 `segment N` 开头（`TestLyricsDocumentSegmentsAreValidated`），例如：

- `the segment ja values must concatenate to the line's ja; they first differ at character 8: the line has "の{歌|うた}", the segments give "{歌|うた}"`：各段拼接后与整行不同，这里的分段 `[{"ja": "{試験|しけん}"}, {"ja": "{歌|うた}"}]` 漏了「の」。消息给出两边从分歧处开始的最多 16 个字符。改了有分段的行的 `ja`（包括注音）时，各段的 `ja` 要一起改。
- `segment 0: unclosed '{' at character 0; write {{ for a literal {`：注音跨了段。
- `segment 1: performer ID 999 is not a known character or audited singer`：演唱者 ID 不存在。
- `segments, when present, must list 1 to 100 segments; it lists 0`：段数不在 1–100 之间。

**配方 6：Game 的 `cut`、`independent`、`only`。**

- `cut`：Game 恰好是 `inGame: true` 的 Full 行，至少一行。
  - 让 `sekai` 第 2 行不进 Game：`{"ja": "{点線|てんせん}をなぞる", "zh": "描过虚线", "performerIds": []}`，得到 `…{"before":2,"after":2,"fields":["inGame"]}…`。
  - 所有 Full 行都标 `inGame` 时，发布结果就是 `same`：`changes` 报 `"game":{"before":"cut","after":"same"}`，并对原来已标 `inGame` 的行各报一条 `inGame`。这是预期结果。
  - 一行都没标：`{"rendition":"sekai","side":"full","field":"inGame","message":"a cut Game needs at least one line with inGame: true"}`。
  - `cut` 带 `gameLines`：`{"rendition":"sekai","side":"game","field":"gameLines","message":"gameLines are only valid when game is independent or only"}`。
- `independent`：Game 用自己的 `gameLines`，Full 和 Game 的 `zh` 各自保存，翻译时两处都要改。
  - 把 `vocaloid` 的 `gameLines[0]` 换成 `{"ja": "ミクのうた", "zh": "初音未来之歌", "stanzaBreakBefore": true}`，得到 `{"against":"served","changed":true,"renditions":[{"key":"vocaloid","sides":[{"side":"game","linesBefore":1,"linesAfter":1,"addedCount":0,"removedCount":0,"changedCount":1,"changed":[{"before":0,"after":0,"fields":["zh"],"zh":{"before":"未来之歌","after":"初音未来之歌"}}]}]}]}`。
  - 改成 `none` 并删去 `gameLines`，得到 `"game":{"before":"independent","after":"none"}` 和一条 `game` 面的 `removed`：`[{"line":0,"ja":"ミクのうた","zh":"未来之歌","stanzaBreakBefore":true,"performerIds":[21]}]`（`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter`）。
- `only`：只有 Game，`lines` 写 `[]`，Game 行写在 `gameLines`（`TestLyricsDocumentGameOnlyRenditionKeepsTheServedState`）。把 `vocaloid` 换成：

  ```json
  {"key": "vocaloid", "kind": "vocaloid", "label": "VIRTUAL SINGER Version", "game": "only", "performerIds": [21], "lines": [],
   "gameLines": [{"ja": "ミクのうた", "zh": "未来之歌", "stanzaBreakBefore": true}]}
  ```

  ```json
  {"against":"served","changed":true,"renditions":[{"key":"vocaloid","game":{"before":"independent","after":"only"},"sides":[{"side":"full","linesBefore":2,"linesAfter":0,"addedCount":0,"removedCount":2,"changedCount":0,"removed":[{"line":0,"ja":"ミクのうた","zh":"未来之歌","performerIds":[21]},{"line":1,"ja":"ルルル","zh":"噜噜噜","performerIds":[21]}]}]}]}
  ```

  `only` 的 `lines` 不为空时得到 `{"rendition":"vocaloid","side":"full","field":"lines","message":"a Game-only rendition has no lines; its lines go in gameLines"}`。

**配方 7：字面的花括号。** 公开的日文要显示 `らららと{合成}`，其中 `合成` 带读音 `ごうせい`：`{{` 写字面的 `{`，接着是注音 `{合成|ごうせい}`，最后 `}}` 写字面的 `}`。`zh` 原样保存，不转义（`TestLyricsDocumentMarkupEscapesLiteralBraces`）。

```json
{"ja": "らららと{{{合成|ごうせい}}}", "zh": "啦啦啦地{合成}", "stanzaBreakBefore": true, "performerIds": [1]}
```

`…{"before":1,"after":1,"fields":["ja","zh"],"ja":{"before":"らららと{合成|ごうせい}","after":"らららと{{{合成|ごうせい}}}"},"zh":{"before":"啦啦啦地合成","after":"啦啦啦地{合成}"}}…`

单独的花括号会被拒收：`らららと{合成|ごうせい}}` 报 `unbalanced '}' at character 13; write }} for a literal }`，`{らららと{合成|ごうせい}` 报 `unclosed '{' at character 0; write {{ for a literal {`。

**配方 8：改译文。** 把 `sekai` 第 1 行的 `zh` 改成 `啦啦啦合成了`，得到 `…{"before":1,"after":1,"fields":["zh"],"zh":{"before":"啦啦啦地合成","after":"啦啦啦合成了"}}…`（`TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest` 断言的就是这条）。

**配方 9：署名。**

- 改文档级署名：`"proofreadingCredit": "新的校对"` 会改变每个用文档署名的 rendition：

  ```json
  {"against":"served","changed":true,"renditions":[{"key":"sekai","translationCredits":{"before":{"translation":"合成译者","proofreading":"合成校对"},"after":{"translation":"合成译者","proofreading":"新的校对"}}},{"key":"vocaloid","translationCredits":{"before":{"translation":"合成译者","proofreading":"合成校对"},"after":{"translation":"合成译者","proofreading":"新的校对"}}}]}
  ```
- 给一个 rendition 单独署名：在 `vocaloid` 上加 `"translationCredits": {"translation": "另一位译者"}`，只有它变：`{"against":"served","changed":true,"renditions":[{"key":"vocaloid","translationCredits":{"before":{"translation":"合成译者","proofreading":"合成校对"},"after":{"translation":"另一位译者"}}}]}`。写 `{}` 则 `after` 为 `{}`，这个 rendition 公开时不署名（`TestLyricsDocumentRenditionCreditsReplaceTheDocumentCredits`）。
- 去掉文档级署名，但没有让每个带 `zh` 的 rendition 自带署名：`{"rendition":"","field":"translationCredit","message":"translationCredit or proofreadingCredit is required: rendition(s) sekai, vocaloid have zh lines and no translationCredits of their own"}`。每个带 `zh` 的 rendition 都带 `translationCredits` 后，文档级署名可以都省略（`TestPublishLyricsDocumentNeedsDocumentCreditsOnlyForRenditionsWithoutTheirOwn`）。
- 最后没有任何 rendition 带署名：`{"rendition":"","field":"translationCredit","message":"at least one rendition needs a translation or proofreading credit to publish; the public site serves no song without one"}`。

**配方 10：换来源修订。** `source.url` 的 `oldid=4242` 改成 `4243`，得到 `{"against":"served","changed":true,"source":{"before":{"url":"https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242","title":"合成試験曲"},"after":{"url":"https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4243","title":"合成試験曲"}}}`。去掉 `oldid` 得到 `422 source_revision_required`，详情为 `source revision is invalid: source.url must pin one revision with ?oldid=<revision id>`。

**配方 11：从头写一首新歌。** GET 返回 `404` 时，把基准文档的 `document` 当模板：换成自己的 `musicId`、`source`、署名和各行，`expectedRevision` 填 404 响应的 `current.revision`（全新的歌是 `0`）。第一次 dryRun 的 `changes` 为 `against: "nothing"`，`source.before` 为 `null`，所有 rendition 都在 `renditionsAdded` 里（`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter`）。

**配方 12：多译本的歌。** 一首歌可以有多个 `zh-CN` 译本，控制台可以新建、复制、改名和设为默认。例如歌曲 682 有 `main`（名称 `雪莹ちゃん`，默认）和 `aishitenryu`（名称 `爱死天流`）两个译本。GET 导出全部译本，原样提交导出的 `document` 时，`changes` 为 `{"against":"served","changed":false}`，两个译本的内容和署名都不变（`TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail`、`TestPublishLyricsDocumentKeepsTheTranslationEditionsOfSong682`）。第一次提交会按整曲文档的规则重写两项元数据：`sourceTabPaths` 改为版本名（682 从 `Full Version` 变为 `SEKAI Version`），`provenance` 多出注音一项（`full_ruby`）；`changes` 不把它们算作改动，之后再原样提交，公开详情除 `revision`、`updatedAt` 外逐字节不变。

本配方的基准：在上面的基准文档上加一个非默认译本 `alt`，按 `lyricsDocumentEditionsTestRequest` 发布。GET 返回 `"servedVersion": 4`、`"warnings": []`，`document` 如下：

```json
{
  "musicId": 901,
  "expectedRevision": 2,
  "source": {"url": "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242", "title": "合成試験曲"},
  "translationCredit": "合成译者",
  "proofreadingCredit": "合成校对",
  "translationEditions": [{"key": "main", "label": "合成主译本"}, {"key": "alt", "label": "合成别译本"}],
  "renditions": [
    {"key": "sekai", "kind": "sekai", "label": "SEKAI Version", "game": "cut",
     "editionCredits": {"alt": {"translation": "别译者", "proofreading": "别校对"}},
     "performerIds": [1, 2], "lines": [
      {"ja": "{試験|しけん}の{歌|うた}", "zh": "测试之歌", "zhEditions": {"alt": "别译测试之歌"}, "inGame": true},
      {"ja": "らららと{合成|ごうせい}", "zh": "啦啦啦地合成", "zhEditions": {"alt": "别译啦啦啦"}, "stanzaBreakBefore": true, "performerIds": [1]},
      {"ja": "{点線|てんせん}をなぞる", "zh": "描过虚线", "inGame": true, "performerIds": []}
    ]},
    {"key": "vocaloid", "kind": "vocaloid", "label": "VIRTUAL SINGER Version", "game": "independent", "performerIds": [21], "lines": [
      {"ja": "ミクのうた", "zh": "未来之歌"},
      {"ja": "ルルル", "zh": "噜噜噜"}
    ], "gameLines": [
      {"ja": "ミクのうた", "zh": "未来之歌", "zhEditions": {"alt": "别译游戏版未来之歌"}, "stanzaBreakBefore": true}
    ]}
  ]
}
```

读法：

- `translationEditions` 的第一项是默认译本，这里是 `main`。各行的 `zh`、文档级署名和 rendition 的 `translationCredits` 都属于默认译本。
- 其他译本（这里是 `alt`）的译文写在行的 `zhEditions` 里，署名写在 rendition 的 `editionCredits` 里。
- 缺少某个 key 表示那个译本里没有这项：`sekai` 第 2 行和 `vocaloid` 的 `lines` 在 `alt` 里为空；`vocaloid` 没有 `editionCredits`，表示它在 `alt` 里不署名。
- `cut` 的 Game 跟随 Full 行，所以 `sekai` 没有 `gameLines`；`independent` 的 `gameLines` 带自己的 `zhEditions`。

改日文或注音，保留每个译本：整行替换时把 `zhEditions` 一起抄上。例如把 `sekai` 第 0 行的注音拆开：

```json
{"ja": "{試|し}{験|けん}の{歌|うた}", "zh": "测试之歌", "zhEditions": {"alt": "别译测试之歌"}, "inGame": true}
```

`…{"before":0,"after":0,"fields":["ruby"],"ja":{"before":"{試験|しけん}の{歌|うた}","after":"{試|し}{験|けん}の{歌|うた}"}}…`，公开的每个译本都保留（`TestLyricsDocumentRubyFixKeepsEveryTranslationEdition`）。漏抄 `zhEditions` 时，`fields` 为 `["ruby","zhEditions"]`，并带 `"zhEditions":{"alt":{"before":"别译测试之歌","after":""}}`，提交后 `alt` 的这一行会变空。看到这一项，就把 `zhEditions` 补回去再 dryRun。只改日文字面时同理，例如把第 1 行改成 `{"ja": "るるると{合成|ごうせい}", "zh": "啦啦啦地合成", "zhEditions": {"alt": "别译啦啦啦"}, "stanzaBreakBefore": true, "performerIds": [1]}`，`fields` 只有 `ja`。

拆行、合行时，每个译本的译文都要跟着拆开或合并，写进新行各自的 `zh` 与 `zhEditions`。例如把 `sekai` 第 1 行拆成两行：

```json
[{"ja": "らららと", "zh": "啦啦啦", "zhEditions": {"alt": "别译啦"}, "stanzaBreakBefore": true, "performerIds": [1]},
 {"ja": "{合成|ごうせい}", "zh": "地合成", "zhEditions": {"alt": "别译合成"}, "performerIds": [1]}]
```

```json
{"against":"served","changed":true,"renditions":[{"key":"sekai","sides":[{"side":"full","linesBefore":3,"linesAfter":4,"addedCount":1,"removedCount":0,"changedCount":1,"added":[{"line":2,"ja":"{合成|ごうせい}","zh":"地合成","performerIds":[1],"inGame":false,"zhEditions":{"alt":"别译合成"}}],"changed":[{"before":1,"after":1,"fields":["ja","ruby","zh","zhEditions"],"ja":{"before":"らららと{合成|ごうせい}","after":"らららと"},"zh":{"before":"啦啦啦地合成","after":"啦啦啦"},"zhEditions":{"alt":{"before":"别译啦啦啦","after":"别译啦"}}}]}]}]}
```

发布后 `main` 的 `sekai` 各行是 `测试之歌`、`啦啦啦`、`地合成`、`描过虚线`，`alt` 是 `别译测试之歌`、`别译啦`、`别译合成` 和一行空行（`TestLyricsDocumentLineSplitRealignsEveryTranslationEdition`）。

增加、改名、切换默认、删除译本：

- 给只有隐式译本的歌加译本：写出完整列表 `[{"key": "main", "label": "默认译本"}, {"key": "alt", "label": "合成别译本"}]`，再给行加 `zhEditions`、给 rendition 加 `editionCredits`。在配方 1–11 的基准文档上加上面的列表、`sekai` 的 `"editionCredits": {"alt": {"translation": "别译者"}}` 和第 0 行的 `"zhEditions": {"alt": "别译测试之歌"}`，得到：

  ```json
  {"against":"served","changed":true,"translationEditions":{"before":[{"key":"main","label":"默认译本"}],"after":[{"key":"main","label":"默认译本"},{"key":"alt","label":"合成别译本"}]},"renditions":[{"key":"sekai","editionCredits":{"alt":{"before":{},"after":{"translation":"别译者"}}},"sides":[{"side":"full","linesBefore":3,"linesAfter":3,"addedCount":0,"removedCount":0,"changedCount":1,"changed":[{"before":0,"after":0,"fields":["zhEditions"],"zhEditions":{"alt":{"before":"","after":"别译测试之歌"}}}]}]}]}
  ```

  发布后公开详情变为 v4，响应里的 `document` 也是 v4。新译本可以先留空：只在列表里加 `{"key": "draft", "label": "空白译本"}`、不写任何 `zhEditions`，也能提交，`changes` 只有 `translationEditions`。
- 改名：只改列表里的 `label`，`changes` 只有 `"translationEditions":{"before":[…],"after":[…]}`。`label` 首尾的空白会去掉。
- 只改某个译本的一行：在那一行加上或改动 `zhEditions`，例如给 `sekai` 第 2 行加 `"zhEditions": {"alt": "别译描过虚线"}`，得到 `…{"before":2,"after":2,"fields":["zhEditions"],"zhEditions":{"alt":{"before":"","after":"别译描过虚线"}}}…`。只改某个译本的署名：给 `vocaloid` 加 `"editionCredits": {"alt": {"translation": "别译者"}}`，得到 `{"against":"served","changed":true,"renditions":[{"key":"vocaloid","editionCredits":{"alt":{"before":{},"after":{"translation":"别译者"}}}}]}`。
- 切换默认译本：把新的默认译本移到列表第一项，再把两个译本的内容对调。
  - 每行的 `zh` 换成新默认译本的译文，原来的 `zh` 移进 `zhEditions`，以旧默认译本的 key 为键；
  - 新默认译本的署名移到 `translationCredits` 或文档级署名，旧默认译本的署名移进 `editionCredits`。

  把基准的默认译本换成 `alt` 时，`sekai` 写成：

  ```json
  {"key": "sekai", "kind": "sekai", "label": "SEKAI Version", "game": "cut",
   "translationCredits": {"translation": "别译者", "proofreading": "别校对"},
   "editionCredits": {"main": {"translation": "合成译者", "proofreading": "合成校对"}},
   "performerIds": [1, 2], "lines": [
    {"ja": "{試験|しけん}の{歌|うた}", "zh": "别译测试之歌", "zhEditions": {"main": "测试之歌"}, "inGame": true},
    {"ja": "らららと{合成|ごうせい}", "zh": "别译啦啦啦", "zhEditions": {"main": "啦啦啦地合成"}, "stanzaBreakBefore": true, "performerIds": [1]},
    {"ja": "{点線|てんせん}をなぞる", "zhEditions": {"main": "描过虚线"}, "inGame": true, "performerIds": []}
  ]}
  ```

  - `translationEditions` 写成 `[{"key": "alt", "label": "合成别译本"}, {"key": "main", "label": "合成主译本"}]`，并去掉文档级署名。
  - `vocaloid` 同样对调：`lines` 只剩 `zhEditions.main`，`gameLines[0]` 的 `zh` 是 `别译游戏版未来之歌`。它带 `"translationCredits": {}`（在 `alt` 里不署名）和 `"editionCredits": {"main": {"translation": "合成译者", "proofreading": "合成校对"}}`。
  - `changes` 报 `translationEditions` 顺序改变，还会报每行的 `zh` 与 `zhEditions`、每个 rendition 的 `translationCredits` 与 `editionCredits`。`alt` 原来为空的行，`zh` 的 `after` 为 `""`，例如 `"zh":{"before":"描过虚线","after":""}`。
  - 公开 v4 详情里的译本内容不变，只有默认译本变成 `alt`；v3 详情显示新的默认译本（`TestLyricsDocumentAddsRenamesAndSwitchesTheDefaultTranslationEdition`）。
- 删除译本：从列表里删掉它，并删掉它的全部 `zhEditions` 和 `editionCredits`。`changes` 报 `translationEditions` 的变化、那个译本的 `editionCredits`（`after` 为 `{}`），以及每行 `zhEditions` 的 `after` 为 `""`。删到只剩 `main` 时，可以保留 `[{"key": "main", "label": "合成主译本"}]` 来保留译本名。公开站之后提供的是 v3，里面没有译本名，GET 导出和 `changes` 的比较基准会从数据库补上这个列表（4.6）；省略 `translationEditions` 则译本名回到 `默认译本`，`changes` 会报出这一变化。
- 列表必须含 `main`，但默认译本不一定是 `main`。

`PUT` 写入的内容与控制台编辑译本时写入的相同，所以发布后控制台可以继续改名、设默认、按译本保存（`TestConsoleEditionEditingWorksAfterALyricsDocumentPublish`）。

译本相关的 issue 除译本数超限那一条外都带 `edition`（出问题的译本 key），`field` 为 `translationEditions`、`zhEditions`、`editionCredits` 或 `editionCredits.translation` / `editionCredits.proofreading`（`TestPublishLyricsDocumentLocatesEveryTranslationEditionIssue`）：

| `message` | 原因 |
| --- | --- |
| `translationEditions must contain 1 to 16 entries; it has 17` | 译本多于 16 个（这条没有 `edition`） |
| `translationEditions must include the edition main` | 列表里没有 `main`，`edition` 为 `main` |
| `edition key is repeated` | 两项的 `key` 相同 |
| `edition key "Alt!" must match ^[a-z0-9][a-z0-9._-]{0,127}$` | `key` 不合法；引用这个译本的 `zhEditions`、`editionCredits` 还会各报一条 `not declared` |
| `label must be 1 to 256 bytes of UTF-8 after trimming; it has 0 bytes` | `label` 为空或只有空白，或超过 256 字节 |
| `edition "ghost" is not declared in translationEditions` | `zhEditions` 或 `editionCredits` 用了列表里没有的 key，例如 `{"rendition":"sekai","side":"full","line":2,"field":"zhEditions","edition":"ghost","message":"…"}` |
| `edition "main" is the default edition (the first in translationEditions); its text is zh` | `zhEditions` 用了默认译本的 key；默认译本的译文写在 `zh` |
| `edition "main" is the default edition (the first in translationEditions); its credits are translationCredits or the document credits` | `editionCredits` 用了默认译本的 key |
| `zhEditions needs translationEditions, which declares the song's translation editions` | 请求没有 `translationEditions`，却带了 `zhEditions`；`editionCredits` 同样报 `editionCredits needs translationEditions, …` |
| `zhEditions["alt"] must be one line of at most 16384 bytes; it contains a line break or NUL at character 2` | 译文含换行或 NUL；超过 16384 字节时，分号后写的是 `it has N bytes` |
| `editionCredits.translation must be one line of at most 2048 bytes; it contains a line break or NUL at character 1` | 署名含换行或 NUL，或超过 2048 字节；`field` 为 `editionCredits.translation` 或 `editionCredits.proofreading`。首尾空白会先去掉 |

### 4.5.1 一次返回全部问题

校验失败时，`issues` 一次列出全部问题，每条带定位 `rendition`、`side`、`line`、`field`，与译本有关的问题另带 `edition`（译本 key）：

- markup 问题带字符下标和出错原文；
- 行级问题写明原因，例如 `zh must be one line of at most 16384 bytes; it contains a line break or NUL at character 2`；
- rendition 级问题（如 `game "sometimes" must be none, same, cut, independent or only`）不会挡住对其下各行的检查。

最多列出 200 条，超出时末尾追加一条 `field: "issues"`，内容为 `N more issues are not listed; fix the listed ones and send the document again`（`TestPublishLyricsDocumentReportsEveryIssueInOneBoundedResponse`）。

### 4.5.2 补注音：`POST /api/editor/v1/lyrics/document/ruby`

给一批 `ja` markup 里还没有注音的汉字补上词典读音（Kagome / IPADIC，与来源导入生成注音用的是同一套词典）。它不读写任何歌曲，结果只在响应里，要写进 `document` 再按 4.5 提交。

```bash
curl -sS -X POST "$BASE/api/editor/v1/lyrics/document/ruby" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"lines": ["今日も見上げる空", "{試験|シケン}の歌", "{試|し}験"]}'
```

```json
{"generator": "…", "lines": [
  {"ja": "{今日|きょう}も{見上|みあ}げる{空|そら}", "suggested": [{"character": 0, "text": "今日", "reading": "きょう"}, {"character": 9, "text": "見上", "reading": "みあ"}, {"character": 18, "text": "空", "reading": "そら"}], "problems": []},
  {"ja": "{試験|シケン}の{歌|うた}", "suggested": [{"character": 9, "text": "歌", "reading": "うた"}], "problems": []},
  {"ja": "{試|し}験", "suggested": [], "problems": ["kanji without a {kanji|reading}: 「験」 at character 5"]}
]}
```

- `lines`：1–2000 条，每条是一行的 `ja` markup，单行、不超过 8192 字节，写法同 4.3。响应的 `lines` 与请求一一对应。
- 读音按词给出：`{今日|きょう}` 一个注音盖住整个词，送假名留在花括号外（`{見上|みあ}げる`）。
- 已经写了的注音原样保留，其余字符（包括 `{{`、`}}`）也原样保留，只插入新注音。一个词和已写的注音重叠时不补，例如 `{試|し}験` 里的「験」。
- `suggested` 列出新加的注音，`character` 是它的 `{` 在返回的 `ja` 里的字符下标。
- `problems` 是这个 `ja` 按 `PUT` 校验仍会报的 `field: "ja"` 问题：`[]` 表示每个汉字都有注音了；否则列出词典读不出或与已写注音重叠的汉字，要手工补。`ja` 有其他写法问题（例如花括号没闭合）时原样返回，`suggested` 为空，`problems` 列出全部问题。
- **读音是词典的常用读法，不是来源里的读法。** 歌词常有特殊读法，例如「運命」读作「さだめ」、「本気」读作「マジ」，词典会给出「うんめい」「ほんき」。提交前要按来源逐个核对 `suggested`，改掉不对的读音。
- 分段的行：每段单独调用一次，再把各段的结果按顺序拼成整行的 `ja`，这样注音不会跨段（4.5 配方 5）。
- 请求本身写错时返回 `400 invalid_request`，`details` 说明原因，例如 `lines must list 1 to 2000 ja markups; it lists 0`、`lines[1] must be one line of at most 8192 bytes`（`TestLyricsDocumentRubyRouteSuggestsReadingsForAnySignedInUser`、`TestSuggestLyricsDocumentRubyFillsOnlyKanjiWithoutRuby`）。

### 4.6 从数据库导出：`from=database`

`GET …&from=database` 读数据库里可编辑的状态，响应里 `from` 为 `database`；`expectedRevision` 同样按 4.2 计算。适用于以下情形：

- `unpublished_draft_replaced`：要在旧版草稿上继续改，从数据库导出草稿（`TestExportLyricsDocumentFromDatabaseReadsALegacyDraft`）。
- `served_revision_differs` 的原因 ②：数据库里的 source-v3 译文没有署名，所以没有公开。从数据库导出它，补上署名再提交。
- 已撤下的歌：缺省的 `from=served` 也会回落到这里，并带 `withdrawn_republished`。提交后撤下记录被删除，歌曲重新公开。
- 看控制台刚保存、投影还没重建的内容（`TestExportLyricsDocumentFromDatabaseReadsTheEditableState`）。

数据库里有多个译本时全部导出，写法同配方 12；`from=database` 与 `from=served` 得到同一个请求（`TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail`）。只有一个译本、但在控制台改过译本名的歌，公开站提供的是 v3，里面没有译本名；数据库状态与公开内容一致时，`from=served` 导出从数据库补上 `translationEditions`，`changes` 的比较基准也一样，所以原样提交不会丢掉译本名（`TestLyricsDocumentExportKeepsTheLabelOfARenamedOnlyEdition`）。公开站正在提供这首歌时，`PUT` 的 `changes` 仍然与公开内容比较（`against: "served"`），所以会列出数据库版本与公开版本之间的全部差异。只有内嵌包条目、数据库里没有可编辑歌词的歌，`from=database` 返回 `404`，这时用缺省的 `from=served`。

### 4.7 转为可编辑：`POST /api/editor/v1/lyrics/document/takeover`（仅管理员）

```bash
curl -sS -X POST "$BASE/api/editor/v1/lyrics/document/takeover" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"musicId": <id>, "expectedRevision": <GET 返回的 document.expectedRevision>}'
```

- 请求体只接受 `musicId` 和 `expectedRevision` 两个字段，没有 `dryRun`，其他字段返回 `400`。
- 服务端先读取公开站此刻提供的详情，按 4.1 导出，再按 `PUT` 原样发布。发布后的公开内容与之前相同，只在导出警告指出的字段上可能有差异。
- 检查顺序：
  1. 公开站没有提供这首歌时返回 `404 not_found`（`the public site serves no lyrics detail for this musicId, so there is nothing to take over`）。
  2. 之后按 `PUT` 的顺序检查（4.9），所以导出后 `PUT` 会拒收的歌同样返回 422：例如带 `source_unsupported`、`ruby_not_served`、`credit_missing` 警告的歌，或没有任何一行 `zh` 的歌（例如尚未翻译的台账歌曲）。这类歌改走 GET → 修好 `document` → `PUT`。
- `expectedRevision` 与 `PUT` 规则相同：缺少返回 `422 expected_revision_required`，过期返回 `409 revision_conflict`，两者都带 `current.revision`；编辑调用返回 `403`。被拒绝时什么都不写。
- 成功返回 `200`，响应体是 `PUT` 的结果再加上 `warnings`：`{"dryRun": false, "musicId", "revision", "publicPath", "document", "changes", "warnings": [...]}`。
  - `changes` 通常是 `{"against":"served","changed":false}`；
  - `warnings` 就是导出警告，总是数组。它们在接管时已经生效，想先看再决定，就先调用 GET。
- 有多个译本的歌（例如在控制台建过译本的台账歌曲）接管后保留全部译本，公开的 v4 详情不变（`TestTakeOverLyricsDocumentKeepsTheEditionsOfARecoveryLedgerSong`）。
- 测试：`TestLyricsDocumentTakeoverRoutePublishesTheServedSongAsADocument`（404、403、409、422、200，公开各行不变）、`TestTakeOverLyricsDocumentMakesAServedLedgerSongAnEditorDocument`。

什么时候用：

- 恢复导入台账提供的歌：控制台读取的 `GET /api/lyrics/detail?musicId=<id>` 文档带 `"recoveryLedgerOwned": true`，这时日文、注音、分段、演唱者、段落等源层改动都会得到 `source_drift`，只能改译文。接管之后这个字段消失，源层改动可以保存。控制台的「转为可编辑」按钮调用的就是这个端点。
- 旧版发布或内嵌包提供的歌：效果等于 GET 后原样 `PUT`，少一次往返。

### 4.8 恢复导入台账里的歌曲

被恢复导入台账（`lyrics_recovery_import_items`）认领的歌曲可以用 `PUT` 或 takeover 发布，不论台账条目带 source 文档（`complete`、`game_only`）还是只有可用性状态（如 `missing`）。

- 第一次非 dryRun 的发布在同一事务里写一行接管记录（表 `lyrics_recovery_takeovers`，数据库迁移 v38）。
  - 记录记下被取代的台账条目，并逐字保存它原有的恢复 source 文档；只有可用性状态的条目不存文档。
  - 被取代的是 source-v3 文档时，它的全部 `zh-CN` 译文（含译本）一并存进 `localizations_json` 列；没有译文时该列为空。
  - 台账各行本身不改。`dryRun` 在事务里同样写入接管记录，随后与其他写入一起回滚，不留下记录；被拒绝的提交（校验失败、`409`、`422`）不写。
- 接管之后，这首歌就是一首普通的编辑器发布歌曲：
  - 控制台目录不再显示台账的可用性状态（`lyricsAvailabilityState`），歌词编辑器照常加载和保存；
  - 之后的 `PUT` 按普通替换处理，接管记录保持不变，也不会再写新的；没有接口能撤销接管；
  - 离线 `lyrics-recovery-import` 拒绝任何含这首歌条目的新批次。
- 离线恢复导入的这条限制不只针对接管过的歌。source 文档归编辑器所有的歌同样被拒绝：用 `PUT` 或 takeover 发布过的歌、迁移 v32 写入的歌曲 682，以及内嵌编辑器 seed 写入的歌。一个批次必须覆盖整个曲库，所以库里只要有一首这样的歌，新批次就一律被拒绝；重放已导入的批次不受影响。
- 接管记录（含 `localizationsJson`）随内容备份（`translation-content/lyrics.json` 的 `recoveryTakeovers`）导出和恢复。

### 4.9 错误

错误沿用 `{"error": code, "details": [string], "current"?: any}`；校验失败另带 `issues`：

```json
{"error": "invalid_lyrics_document", "details": ["the lyrics document failed validation"],
 "issues": [{"rendition": "sekai", "side": "full", "line": 0, "field": "ja", "message": "kanji without a {kanji|reading}: 「験」 at character 5, 「歌」 at character 7"}]}
```

`PUT` 按这个顺序检查，返回第一个失败：

1. `musicId`
2. `source.url`
3. 目录
4. 文档内容
5. `expectedRevision`（缺少返回 422，不符返回 409）
6. 构造将要公开的详情
7. 写入数据库（dryRun 同样执行后回滚），失败返回 `500 internal_error`

takeover 先检查公开站是否提供这首歌，再按同样的顺序检查。

| 状态 / code | 含义 |
| --- | --- |
| `400 {"error":"invalid body"}` | 请求体不是合法的严格 JSON：有未知字段、重复键或尾随的第二个值 |
| `400 invalid_query` | `GET` 的 `musicId` 不是正整数，或 `from` 不是 `served` / `database` |
| `401` / `403` | 未登录 / `PUT` 或 takeover 的调用者不是管理员 |
| `404 not_found` | `PUT`：`musicId` 不在目录中。`GET`：公开站没有提供这首歌，数据库里也没有可编辑的歌词，`current.revision` 是新建时要发送的 `expectedRevision`。takeover：公开站没有提供这首歌 |
| `409 revision_conflict` | `expectedRevision` 与当前值（4.2）不符，`details` 为 `expectedRevision N does not match the current revision M`，`current.revision` 是当前值。按第 4 节开头流程的第 4 步重新读取后再提交 |
| `422 expected_revision_required` | 歌曲存有或正在提供歌词，请求却没有 `expectedRevision`；`current.revision` 是应发送的值（4.2） |
| `422 invalid_lyrics_document` | 带 `issues` 时是校验失败，按 `issues[]` 逐条修正（4.5.1）：`line` 从 0 开始；与具体行无关的问题（署名、`game`、`key` 等）省略 `line`，`side`、`field` 也可能省略；`rendition` 为空串表示整份文档。只有 `details`、没有 `issues` 时，是缺少 `musicId` 或它不是正整数、整份文档有结构错误，或构造将要公开的详情失败 |
| `422 source_revision_required` | `source.url` 不是受支持站点上的修订链接，或没有唯一的正整数 `oldid`。它排在目录检查之前，所以 `musicId` 不存在、链接也不对时返回的是它 |
| `500 internal_error` | 写入数据库失败。响应体只有 `{"error":"internal_error"}`，原因记在服务端日志里。dryRun 执行同样的写入，所以同样返回它 |
| `503 projection_unavailable` | `GET` 时服务端没有接上公开文件服务 |

## 5. 发布与撤下

- **source-v3 歌曲保存即发布**：对已是 source-v3 文档的歌曲，`PUT /api/editor/v1/lyrics/save`（带 `renditions` 的请求体）保存成功后立即触发公开投影重建，没有单独的“发布”步骤；控制台的协作保存（`POST /api/editor/v1/lyrics/{musicId}/checkpoint`）改动了 source-v3 文档时也一样。例外有三个：
  - 已被撤下的歌曲保存后仍保持撤下，要再调用 `publish`。第 4 节的整首文档路由则会清除撤下标记。
  - 该曲没有任何 rendition 带翻译或校对署名时，重建不会发布这次保存的内容，公开站继续提供原来的版本或不提供这首歌。补上署名后才会公开。
  - 如果只改排版或演唱者，而该曲还没有任何译文或署名，保存会返回 `422 translation_required`。先补一行译文或署名再保存。
- **旧版（非 source-v3）歌词**首存之后仍可修改来源链接、增删和调整行、改日文原文；带 `sourceImportToken` 的 verified 导入仍只能用于首存。
- **撤下**（仅管理员）：`POST /api/editor/v1/lyrics/unpublish {"musicId": <id>, "revision": <当前 revision>}`。歌曲会从 `index.json` 和详情路由消失，即使它在内嵌基线包里；`POST /api/editor/v1/lyrics/publish` 用同样的请求体恢复。`revision` 必须等于当前值（source-v3 歌曲取 `GET /api/lyrics/detail?musicId=<id>` 返回文档的 `revision`），否则 `409 revision_conflict`。`GET /api/catalog/music` 的条目在撤下期间带 `"lyricsWithdrawn": true`，未撤下时省略该字段；source-v3 歌曲的条目带 `"lyricsSourceV3": true`，其他歌曲省略。
- **source-v3 歌曲的 publish / unpublish 响应**：只写入或清除撤下标记，不改文档，也不改 `revision`。
  - 成功时返回 `200`，响应体是该曲当前的 source-v3 文档（`musicId`、`status`、`revision`、`renditions` 等）。其中 `status` 恒为 `"draft"`，也没有 `publishedRevision`，所以响应不反映是否已撤下；公开状态以第 7 节的方法确认。
  - 歌曲已处于目标状态时同样返回 `200`，不写审计，也不触发重建。
  - `revision` 不符时返回 `409 {"error":"revision_conflict","current":<当前文档>}`，用 `current.revision` 重试。
- `POST /api/projection/publish`（任何已登录用户）请求一次立即全量重建并返回投影状态；正常写入已自动触发重建，一般不需要调用。

## 6. 旧路径

`PUT /api/entry`、`PUT /api/lyrics/save`、`POST /api/lyrics/publish` 重新挂载，与 `/api/editor/v1/entry`、`/api/editor/v1/lyrics/save`、`/api/editor/v1/lyrics/publish` 使用同一个包装与处理函数，行为完全相同。新脚本请直接用 `/api/editor/v1/*`；其余旧写路径仍返回 JSON `404`。

## 7. 确认公开文件已更新

1. `GET /api/projection/status?musicId=<id>`：等到 `generation` 大于提交前的值、`pending=false`、`lastError` 为空；`song.revision` 应等于你提交得到的 `revision`，`song.hasDetail=true`。
2. `GET $BASE/files/translation/lyrics/music_<id>.json`：顶层 `revision` 等于提交结果；该文件 `Cache-Control: public, max-age=15, must-revalidate`，经 CDN 读取时最多晚 15 秒，可带上次的 `ETag` 用 `If-None-Match` 比较（未变返回 `304`）。

## 8. 卡牌剧情与区域对话

两类故事共用下面这组路由，`{kind}` 取 `card` 或 `area`：

| `kind` | `{id}` | `{episode}` |
| --- | --- | --- |
| `card`（卡牌剧情） | 卡牌 ID：不以 0 开头的十进制正整数，最多 9 位 | `1`（前篇）、`2`（后篇） |
| `area`（区域对话） | scenarioId，匹配 `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$` | 只有 `1` |

- **语言**：`locale` 只接受 `zh-CN`、`en-US`，两种语言的译文各自独立；日文原文只读。列表、详情、PUT 和 AI 省略 `locale` 时按 `zh-CN` 处理，snapshot 必须带。
- **行**：每话有一个标题行（`role:"title"`，`position:-1`）。TalkData 第 i 条的正文是 `role:"talk"`、`position` 为 2i 的行，说话人是 `role:"speaker"`、`position` 为 2i+1 的行。行键 `jp` 是去掉首尾空白的日文原文，在一话内唯一：同一句出现多次时只有一行，`position` 取第一次出现的位置。talk 行的 `speaker` 是它的说话人行的 `jp`。
- **来源**：`official`（后台回填写入的官方 CN/EN 文本）、`llm`（AI）、`human`（人工）。没有译文的行为 `text:""`、`source:""`、`revision:0`，每写一次 `revision` 加 1。

| 请求 | 权限 | 作用 |
| --- | --- | --- |
| `GET /api/editor/v1/stories?kind=&locale=&status=` | 已登录 | 列出一类故事和翻译进度（8.1） |
| `GET /api/editor/v1/story/{kind}/{id}?locale=` | 已登录 | 读取全部话和行（8.2） |
| `PUT /api/editor/v1/story/{kind}/{id}/{episode}` | 已登录，走第 2 节的门禁 | 写一话里的若干行（8.3） |
| `GET /api/editor/v1/story/{kind}/{id}/{episode}/snapshot?locale=` | 已登录 | 按 TalkData 顺序导出一话，供 TXT 导入（8.4） |
| `POST /api/editor/v1/story/{kind}/{id}/ai` | 管理员 | 用 AI 填空行（8.5） |
| `POST /api/editor/v1/story/{kind}/{id}/refresh` | 管理员 | 立即重抓这个故事的日文和官方脚本（8.6） |
| `GET /api/editor/v1/stories/sync` | 已登录 | 后台回填的状态与总量（8.7） |
| `POST /api/editor/v1/stories/sync` | 管理员 | 立即跑一轮回填（8.7） |

共同的错误：未登录 `401 {"error":"unauthorized"}`；编辑调用管理员路由 `403 {"error":"admin role required"}`；方法不对返回 JSON `404 {"error":"not found"}`；路径或查询参数不合法返回 `400 invalid_request`，`details` 为 `kind must be card or area`、`invalid card story id`、`invalid area episode`、`locale must be zh-CN or en-US` 这类文本；故事不存在返回 `404 {"error":"not_found"}`。服务端没有接上回填 worker 时，snapshot、AI、refresh 和两条 sync 路由返回 `503 side_story_unavailable`，列表、详情和 PUT 照常工作。POST 路由都要求 JSON 请求体，没有参数时发送 `{}`，空请求体返回 `400 {"error":"invalid body"}`。

### 8.1 列表：`GET /api/editor/v1/stories`

```bash
curl -sS "$BASE/api/editor/v1/stories?kind=card&locale=zh-CN&status=untranslated" -H "Authorization: Bearer $TOKEN"
# 200 {"kind":"card","locale":"zh-CN","stories":[{"kind":"card","id":"501","title":"テストカード","characterId":1,"areaId":0,"areaCategory":"",
#   "actionSetId":0,"releasedAt":1790000000000,"episodeCount":2,"fetchedEpisodeCount":1,"lineCount":7,"translatedCount":0,"untranslatedCount":7,
#   "sourceCounts":{"official":0,"llm":0,"human":0},"primarySource":"","status":"untranslated","updatedAt":1790000000}]}
```

- `kind` 必填。`status` 可选，取 `pending`（还没抓到任何一话的日文脚本）、`untranslated`、`partial`、`translated`，其他值返回 400。
- 计数包含标题行。`primarySource` 是已译行里最多的来源，并列时 human > official > llm，没有已译行时为 `""`。`releasedAt` 是 Unix 毫秒，`updatedAt` 是最近一次写译文的 Unix 秒。area 故事另带 `areaId`、`areaCategory`（如 `grade1`）和 `actionSetId`。

### 8.2 详情：`GET /api/editor/v1/story/{kind}/{id}`

```bash
curl -sS "$BASE/api/editor/v1/story/card/501?locale=zh-CN" -H "Authorization: Bearer $TOKEN"
# 200 {"kind":"card","id":"501","title":"テストカード","characterId":1,"areaId":0,"areaCategory":"","actionSetId":0,"locale":"zh-CN","episodes":[
#   {"key":"1","scenarioId":"test_card_501_01","title":"テスト話1","fetched":true,"scriptSha256":"306e4e1d…","cnState":"absent","enState":"absent","lines":[
#     {"jp":"テスト話1","role":"title","position":-1,"text":"","source":"","revision":0},
#     {"jp":"テスト台詞一","role":"talk","speaker":"テスト話者甲","position":0,"text":"","source":"","revision":0},
#     {"jp":"テスト話者甲","role":"speaker","position":1,"text":"","source":"","revision":0}, …],"translatedCount":0,"untranslatedCount":6},
#   {"key":"2","scenarioId":"test_card_501_02","title":"テスト話2","fetched":false,"scriptSha256":"","cnState":"absent","enState":"absent",
#    "lines":[{"jp":"テスト話2","role":"title","position":-1,"text":"","source":"","revision":0}],"translatedCount":0,"untranslatedCount":1}]}
```

- `fetched:false` 表示日文脚本还没抓到，这一话只有标题行。有译文的行另带 `updatedBy` 和 `updatedAt`（Unix 秒）。
- `cnState` / `enState` 是官方 CN / EN 文本的导入状态：`pending`（等待导入；官方脚本返回 404 或返回的仍是日文时也是 `pending`，见 8.8）、`imported`、`absent`（该服务器的 masterdata 没有这一话的脚本路径）、`mismatch`（官方脚本的 TalkData 条数与日文不同，见 8.8）、`error`。最近一次失败记在 `lastError`。

### 8.3 写行：`PUT /api/editor/v1/story/{kind}/{id}/{episode}`

请求体 `{locale?, lines:[{jp, text, source?, expectedRevision?}], clientId?}`：

- `lines` 1–2000 条，同一 `jp` 只能出现一次，且必须是这一话的行键（取自 8.2 或 8.4）。
- `text` 是最多 16384 字节、不含 NUL 的 UTF-8。空串也是合法的人工译文：这一行仍算未翻译，回填和 AI 都不会再填它。
- `source` 省略或写 `human` 时存为 `human`，也可以写 `llm`；`official` 只由回填写入，请求里出现返回 400。
- `expectedRevision` 填读取时的 `revision`，没有译文的行填 0。省略时不做检查、直接覆盖，所以 agent 每行都要带。
- `clientId` 最多 128 字节，原样放进 SSE 事件，发起方用它认出自己的保存。

```bash
curl -sS -X PUT "$BASE/api/editor/v1/story/card/501/1" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"locale":"zh-CN","lines":[{"jp":"テスト台詞一","text":"测试台词一","expectedRevision":0},{"jp":"テスト話者甲","text":"测试说话人甲","expectedRevision":0}],"clientId":"agent-test-1"}'
# 200 {"status":"ok","kind":"card","id":"501","episode":"1","locale":"zh-CN","updated":2,"unchanged":0,"lines":[
#   {"jp":"テスト台詞一","role":"talk","speaker":"テスト話者甲","position":0,"text":"测试台词一","source":"human","revision":1,"updatedBy":"<账号>","updatedAt":1790250412},
#   {"jp":"テスト話者甲","role":"speaker","position":1,"text":"测试说话人甲","source":"human","revision":1,"updatedBy":"<账号>","updatedAt":1790250412}]}
```

- 响应的 `lines` 是每条提交行写入后的状态，下次提交用这里的 `revision`。文本和来源都没变的行计入 `unchanged`，`revision` 不变。
- `updated` 大于 0 时，响应返回前这个故事的公开文件已经重建（8.9），并广播 SSE `sidestory.updated`：`{kind,id,episode,locale,action:"update",lines,user,clientId}`。`updated` 为 0 时既不重建也不广播。area 故事的写法相同，例如 `PUT /api/editor/v1/story/area/areatalk_test_01/1`，请求体 `{"locale":"en-US","lines":[{"jp":"テスト区域台詞","text":"Test area line","expectedRevision":0}]}`。
- 整批全有或全无，下列任何一种错误都不写入：

| 状态 / code | 含义 | 处理 |
| --- | --- | --- |
| `422 unknown_lines` | `current.lines` 列出这一话里不存在的 `jp`：`{"current":{"lines":["存在しない台詞"]},"error":"unknown_lines"}`。它先于修订检查 | 重新读取 8.2，改用现有行键。日文脚本变化后行集会变 |
| `409 revision_conflict` | `current.conflicts` 列出 `expectedRevision` 不符的行和它们的当前状态：`{"current":{"conflicts":[{"jp":"テスト台詞一","expectedRevision":0,"currentRevision":1,"currentText":"测试台词一","currentSource":"human"}]},"error":"revision_conflict"}` | 对照 `currentText` 决定保留哪一版再提交。只把 `expectedRevision` 换成 `currentRevision` 重发，会覆盖别人的修改 |
| `409 {"error":"producer is running; reload before saving"}` | producer（CN 同步、AI 翻译、备份恢复等）正在运行 | 按第 2 节等它结束 |
| `400 invalid_request` | `details` 如 `side story request is invalid: 0 edits, want 1 through 2000`、`side story request is invalid: edit 1 repeats line "テスト台詞二"`、`side story request is invalid: edit 0 source "official"`、`clientId must be at most 128 bytes` | 修正请求 |

### 8.4 快照：`GET /api/editor/v1/story/{kind}/{id}/{episode}/snapshot`

```bash
curl -sS "$BASE/api/editor/v1/story/card/501/1/snapshot?locale=zh-CN" -H "Authorization: Bearer $TOKEN"
# 200 {"kind":"card","id":"501","episode":"1","locale":"zh-CN","revision":"85b67ac0…","segments":[
#   {"id":"テスト台詞一","kind":"talk","position":0,"japanese":"テスト台詞一","sourceHash":"","text":"测试台词一","source":"human","revision":1},
#   {"id":"テスト話者甲","kind":"talk","position":1,"japanese":"テスト話者甲","sourceHash":"","text":"测试说话人甲","source":"human","revision":1},
#   {"id":"テスト台詞二","kind":"talk","position":2,"japanese":"  テスト台詞二  ","sourceHash":"","text":"","source":"","revision":0},
#   {"id":"テスト話者乙_制服","kind":"talk","position":3,"japanese":"テスト話者乙","sourceHash":"","text":"","source":"","revision":0},
#   {"id":"テスト台詞一","kind":"talk","position":4,"japanese":"テスト台詞一","sourceHash":"","text":"测试台词一","source":"human","revision":1}, …],
#   "scenario":{"scenarioId":"test_card_501_01","fileName":"test_card_501_01.json","sha256":"306e4e1d…","parserVersion":1,"rawJson":"…","sourceTalks":[…]}}
```

- 服务端现抓日文脚本，确认其 SHA-256 等于详情里的 `scriptSha256` 才返回。TalkData 第 i 条生成两个 segment：`position` 2i 是正文，`japanese` 为未去空白的 `Body`；`position` 2i+1 是说话人，`japanese` 为 `WindowDisplayName` 第一个 `_` 之前的部分。`id` 是对应行的 `jp`，`text`、`source`、`revision` 是该行当前的译文。重复的句子每次出现都有一个 segment，`id` 相同；正文或说话人为空时不生成 segment。
- `revision` 是不透明字符串，日文脚本或这一话任何一行的 revision 变化时它就变。`scenario` 与活动剧情快照的形状相同。
- 写回时整话用一个 PUT，每个 `id` 只提交一次：`jp` 填 `id`，`expectedRevision` 填该 segment 的 `revision`。
- 错误：`409 script_not_fetched`（日文脚本还没抓到）；`409 script_changed`（上游日文脚本变了，先调用 8.6 再导入）；`502 upstream_unavailable`（抓不到日文脚本）；缺少 `locale` 返回 `400 invalid_request`。

### 8.5 AI 翻译：`POST /api/editor/v1/story/{kind}/{id}/ai`（仅管理员）

```bash
curl -sS -X POST "$BASE/api/editor/v1/story/card/501/ai" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"locale":"en-US","episode":"1","provider":"","clientId":"agent-test-1"}'
# 200 {"translated":3,"remaining":0}
```

- 请求体 `{locale?, episode?, provider?, clientId?}` 的字段都可以省：`{}` 表示 `zh-CN`、全部话、设置 `llm.type` 里的 provider（默认 `openai`）。`provider` 只接受 `gemini`、`openai`。
- 只填没有译文行的行，以及文本为空且来源不是 `human` 的行，写成 `source:"llm"`；列出目标之后被改过的行不会被覆盖。后台回填和 refresh 都不调用 LLM。
- 请求同步执行，作为 producer 任务 `ai-side-story` 运行，期间 PUT 按第 2 节返回 409。`translated` 是写入的行数，`remaining` 是结束后仍可由 AI 填的行数。完成后重建公开文件，并广播 `sidestory.updated`（`action:"ai"`，不带 `lines`）。
- 错误：`409 already_running`（已有翻译任务或 producer 在运行）、`503 draining`（服务正在关闭）、`404 not_found`、`400 invalid_request`（`ja-JP`、不存在的话号、过长的 `clientId`）、`500 internal_error`（LLM 调用失败或 provider 不受支持，原因在 `details`）。

### 8.6 立即重抓：`POST /api/editor/v1/story/{kind}/{id}/refresh`（仅管理员）

```bash
curl -sS -X POST "$BASE/api/editor/v1/story/card/501/refresh" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"clientId":"agent-test-1"}'
# 200 {"episodes":[
#   {"kind":"card","id":"501","key":"1","fetched":true,"scriptChanged":false,"cnState":"imported","enState":"absent","officialWritten":5,"droppedHumanLines":0},
#   {"kind":"card","id":"501","key":"2","fetched":true,"scriptChanged":true,"cnState":"mismatch","enState":"absent","officialWritten":0,"droppedHumanLines":1,"error":"zh-CN: TalkData length mismatch (12 != 11)"}]}
```

- 不等后台回填，立即抓这个故事每一话的日文、CN、EN 脚本并按 8.8 的规则导入。`scriptChanged:true` 表示日文脚本和上次不同，这一话的行集按新脚本替换：仍然存在的行保留译文，消失的行连同译文删除，`droppedHumanLines` 是其中人工译文的条数。`officialWritten` 是写入的官方译文条数；`error` 带语言前缀，如 `ja-JP: …`、`zh-CN: …`。
- 成功后重建公开文件，并广播 `sidestory.updated`（`action:"refresh"`，`episode` 与 `locale` 为 `""`）。
- 错误：`404 not_found`、`409 already_running`（producer 正在运行）、`503 draining`、`502 upstream_unavailable`（没有一话抓到日文脚本；失败仍记进各话的 `lastError`）。

### 8.7 回填状态与触发：`GET` / `POST /api/editor/v1/stories/sync`

```bash
curl -sS "$BASE/api/editor/v1/stories/sync" -H "Authorization: Bearer $TOKEN"
# 200 {"state":{"enabled":true,"running":false,"lastRoundAt":"2026-09-24T08:00:00Z","nextRoundAt":"2026-09-24T08:01:00Z","catalogRefreshedAt":"2026-09-24T06:00:00Z",
#   "lastRound":{"episodes":30,"requests":58,"fetched":28,"officialWritten":911,"errors":2,"retrying":5}},"totals":{"card":{"stories":2,"episodes":3,"fetched":1,"pendingFetch":2,
#   "cnImported":0,"cnPending":0,"cnAbsent":3,"cnMismatch":0,"cnError":0,"enImported":0,"enPending":0,"enAbsent":3,"enMismatch":0,"enError":0,"errors":0},"area":{…}}}
curl -sS -X POST "$BASE/api/editor/v1/stories/sync" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"refreshCatalog":true}'
# 202 {"started":true,"state":{…}}
```

- `enabled` 要求环境变量 `SIDE_STORY_BACKFILL_ENABLED` 不为 false，且设置 `side_story_backfill.enabled` 不为 false（未设置即为开，每轮都读取）；它与 `scheduler.enabled` 无关。时间是 RFC 3339 UTC，未知时省略；上一轮出错时带 `lastRoundError`，producer 运行时它是 `a producer job is running; retrying next round`。`totals.<kind>.errors` 是记着日文抓取错误、仍待抓取的话数。`lastRound.errors` 是上一轮日文没抓到、或某语言变成 `error`/`mismatch` 的话数；`lastRound.retrying` 是其余带 `lastError` 的话数，即某语言因 404、镜像仍返回日文或暂时性错误而保持 `pending`、等待自动重试。
- POST 立即唤醒一轮，正在跑的一轮结束后接着跑；`refreshCatalog:true` 让这一轮先重建目录。回填被禁用时返回 `409 backfill_disabled`（`details` 为 `the side-story backfill is disabled`）。
- 每轮写入了内容时广播 SSE `sidestory.sync`，负载为 `{detail,current,total}`，`current` 是抓到的话数，`total` 是本轮处理的话数。

### 8.8 回填导入什么、从不覆盖什么

- **节奏**：每 `SIDE_STORY_BACKFILL_INTERVAL_MS`（默认 60000）跑一轮，每轮最多 `SIDE_STORY_BACKFILL_BATCH`（默认 30）话，每两次上游请求之间至少间隔 `SIDE_STORY_BACKFILL_REQUEST_DELAY_MS`（默认 1000）。producer 运行时整轮推迟到下一轮。
- **目录**：首轮、距上次满 6 小时、上游数据版本变化、`refreshCatalog:true` 时，以及内容备份恢复之后，从 JP 的 `cards.json`、`cardEpisodes.json`、`actionSets.json`、`areas.json`，CN 的 `cards.json`、`cardEpisodes.json`、`actionSets.json`，以及 EN 的 `cardEpisodes.json`、`actionSets.json` 重建。新故事和新话入队；后来从 masterdata 消失的故事保留。CN/EN 的脚本路径变化时重新排队导入，并清零这一话的重试计时，新路径下一轮就抓，不等旧路径的 404 重试；路径变空标为 `absent`；JP 路径变化时重抓日文。目录刷新失败 10 分钟后再试。官方话标题也在这一步按下面的官方写入规则写入。JP 话标题变了时，旧标题行连同它的全部译文一起删除，人工译文也不例外；删掉了人工译文时，服务器日志记一行 `[side-story] <kind> catalog: changed JP episode titles deleted <N> human title translation(s)`。
- **每话**：先抓日文脚本，成功后再抓该服务器有、且状态为 `pending` 的 CN / EN 脚本。日文抓取的临时错误按 10 分钟起翻倍、最长 24 小时退避；日文 404 在 24 小时后重试；日文失败时这一话不导入 CN/EN。CN / EN 脚本返回 404（镜像还没同步）时，该语言保持 `pending`，`lastError` 为 `zh-CN: not found` 或 `en-US: not found`，24 小时后重试，不计入重试次数；CN / EN 的临时错误同样保持 `pending`，按上面的退避重试，两种情况同时出现时以较早的时间为准。已导入过的日文脚本变化时，这次没抓的 CN / EN 回到 `pending`，下一轮就抓，另一种语言这次 404 也不必等 24 小时；另一种语言的临时错误仍按它的退避。
- **配对**：脚本按资源路径识别，脚本内的 `ScenarioId` 字段只是标签，日文和官方脚本都不比较它（真实脚本里有 `016048_rui01 のコピー` 这样的值）。官方脚本 TalkData 第 i 条对应日文第 i 条，正文取 `Body`，说话人取 `WindowDisplayName`。官方文本为空，或与含假名的日文行键完全相同，就跳过这一行。TalkData 条数不同（`TalkData length mismatch (a != b)`）时，这一话该语言标为 `mismatch`，一行都不写，也不按时间重试；可以用 8.6 立即重抓。超过一半的含假名正文与日文相同时（`official script repeats the Japanese text`，镜像站还在提供新上架脚本的日文占位），同样一行都不写，但该语言保持 `pending`，像 404 一样 24 小时后重试。
- **官方写入规则**：没有译文行时插入 `source:"official"`、`revision:1`、`updatedBy:"sync"`；已有 `official` 或 `llm` 行且文本不同时覆盖，`revision` 加 1；**`human` 行从不改动**，包括文本为空的人工行。
- 回填写入后（目录刷新新增了故事或话、重新排队了官方导入、写了官方标题或替换了标题也算）请求一次去抖的全量重建，公开文件稍后更新，按第 7 节的 `GET /api/projection/status` 确认。去抖窗口是服务器的 `FILES_REBUILD_DEBOUNCE_MS`（默认 300000 ms），每次改动重新计时，但从开始等待算起最多两个窗口，所以回填持续写入时，默认最迟 10 分钟也会开始重建。

### 8.9 公开文件

| 语言 | 卡牌剧情 | 区域对话 |
| --- | --- | --- |
| `zh-CN` | `$BASE/files/translation/cardStory/card_<cardId>.json` | `$BASE/files/translation/areaTalk/group_<n>.json` |
| `en-US` | `$BASE/files/v2/en-US/translation/cardStory/card_<cardId>.json` | `$BASE/files/v2/en-US/translation/areaTalk/group_<n>.json` |

`n` 是 JP actionSet ID 整除 100。`/translation/…` 也可以代替 `/files/translation/…`。没有其他语言的镜像。只有至少一条正文或说话人行有非空译文时才有文件，否则返回 `404`。缓存头是 `Cache-Control: public, max-age=300, stale-while-revalidate=3600`，带强 `ETag`。

```json
{"meta": {"source": "human", "version": "1", "last_updated": 1790003600},
 "episodes": {
   "1": {"scenarioId": "test_card_700_01", "title": "测试前篇", "source": "official_cn",
         "talkData": {"テスト台詞一 & <": "测试台词一 & <", "テスト話者": "测试说话人", "テスト台詞二": "测试台词二"}},
   "2": {"scenarioId": "test_card_700_02", "title": "", "source": "human", "talkData": {"テスト台詞三": "测试台词三"}}}}
```

- 实际文件用两空格缩进、不做 HTML 转义（`TestSideStoryFilesJSONBytes`）。卡牌文件的 `episodes` 键是 `"1"`、`"2"`；区域文件的键是 scenarioId，按 actionSet ID 排序。`talkData` 按位置顺序把有译文的正文和说话人行键映射到译文；`title` 是译后标题，没有时为 `""`。
- `source`（每话）和 `meta.source`（整个文件）：已译行（含标题）全部来自官方时为 `official_cn` 或 `official_en`，有任何人工行时为 `human`，否则为 `llm`。`meta.last_updated` 是最近一次写入的 Unix 秒。
