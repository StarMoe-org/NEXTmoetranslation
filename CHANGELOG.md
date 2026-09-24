# 更新日志

## 修复轮（remediation-20260922）

这一轮让 agent 脚本能直接通过 HTTP 读取、提交并发布整首歌词（恢复导入台账里的歌曲也可以），让旧版歌词在首存之后仍可编辑，新增 `gachaInfo` 分类，并修复备份、协作和控制台的一批缺陷。本轮新增数据库迁移 v37 与 v38，都只能前进。

### 部署须知

- 数据库 schema v37 `lyrics_source_artifacts_projectsekai_fandom_origin`：只重建 `song_lyrics_source_artifacts` 一张表。provider `vocaloid_fandom` 除 `https://vocaloid.fandom.com` 外也接受 `https://projectsekai.fandom.com`，其余表定义、索引、三个触发器和全部行都原样保留；迁移在关闭外键的事务里执行，提交前跑 `PRAGMA foreign_key_check`。`TestMigrationsAfterV34MustNotMutateContentTables` 只为 v37 放行这一次逐列复制重建，`TestMigrationV37RebuildsSongLyricsSourceArtifactsVerbatim` 逐表比对迁移前后的行和 schema 对象。在含 787 行 artifact 的内嵌 seed 夹具上，打开数据库并完成 v37 与 v38 约 0.8 s（含迁移前备份和完整性检查）。
- 数据库 schema v38 `lyrics_recovery_takeovers`：只新建 `lyrics_recovery_takeovers` 一张表，外加一个索引和三个只做 RAISE 的触发器，不写任何行，也不改已有表（`TestMigrationV38OnlyCreatesTheTakeoverTable`）。表里的可空列 `localizations_json` 在第 8c 波原地加入，此时 v38 还没有部署到任何环境。`TestMigrationsAfterV34MustNotMutateContentTables` 把 `lyrics_recovery_` 前缀也算作内容表，只为 v38 放行这张表的 `CREATE TABLE`、`CREATE INDEX` 和只含 RAISE 的触发器（`TestCreateOnlyStatementAllowsOnlyTheNamedTableIndexAndRaiseTriggers`）。一旦有接管记录，内容备份的 `translation-content/lyrics.json` 就带 `recoveryTakeovers` 字段，旧版本恢复时会按未知字段拒收。
- `main` 目前停在 v34，所以这个分支的首次部署走 v34→v38：一次启动依次跑完 v35–v38，只在 v35 之前自动生成一份 `/data/moesekai.db.pre-migration-v35.bak`。部署与回滚步骤见 `ROLLBACK_RUNBOOK.md`。
- `workspaceverify` 的外部 workspace manifest 从 schema 3 升到 4，编辑门禁合同 version 从 2 升到 3；`producerProof` 从布尔值改为 `none` / `optional` / `required`。按 schema 3 构建的 SekaiText-Moe workspace 产物会被验证器拒绝。生产使用 `WORKSPACE_MODE=disabled`，不受影响。
- 恢复早于 `gachaInfo` 的备份时，该分类按空分类恢复，会清掉当前的 `gachaInfo` 词条，这与其他分类的恢复语义相同。

### 写入接口与门禁

- 为 agent 脚本重新挂载 `PUT /api/entry`、`PUT /api/lyrics/save`、`POST /api/lyrics/publish`，与对应的 `/api/editor/v1/*` 路由共用同一个鉴权包装和处理函数；其余六条无版本号写路由仍返回 JSON 404（`TestAgentAliasRoutesMatchTheirV1Twins`）。
- 内容写入的 `X-Moe-Loaded-Producer-State` 改为可选。不带时 `strictContentMutation` 按宽松门禁处理：取得编辑准入并持有共享内容锁，producer 运行中返回 409；带了但畸形返回 400，过期返回 409，与之前相同。只有 `POST /api/editor/v1/lyrics/{musicId}/collab-ticket` 与 `POST /api/editor/v1/backup/push` 仍强制该头，缺少时返回 428。`workspaceverify` 的路由合同和 `api_test.go` 也按这套规则更新（`TestStrictContentMutationWithoutHeaderUsesLenientAdmission`、`TestContractStatesLenientContentAdmissionAndRequiredBackupProof`）。
- 不带该头的 verified 导入保存不再返回 428，导入授权改为与保存时观察到的门禁状态比对（`TestLyricsSourceImportSaveWithoutProducerHeader`）。

### 歌词

- 新增 `PUT /api/editor/v1/lyrics/document`（仅管理员）：一次提交整首歌，写成 source-v3 文档加 `zh-CN` 本地化并直接发布，响应里的 `document` 与公开详情逐字节一致。
  - `dryRun` 执行与正式提交相同的写入后回滚，不留下任何改动（第 8d 波起，见下文）。注音写成 `{漢字|かな}`。至少要有一行 `zh` 和一个署名，`en` 非空会被判为 issue。
  - 被恢复导入台账认领的歌曲在首次发布时由本路由接管，见下文「恢复台账接管与导出」。
  - 替换时清除撤下标记，把该曲的内嵌 seed 台账项从 `inserted` 改记为 `preserved_existing`，并立即关闭该曲所有旧的协作房间。
  - 测试：`TestLyricsDocumentRoutePublishesTheServedV3Detail`、`TestDocumentPublishClosesTheLiveRoomOfTheFencedEpoch`。用法见 `contracts/editor-api/README.md`。
- 支持 `projectsekai.fandom.com` 作为 `vocaloid_fandom` 的来源站点：涉及 `LyricsSourceProviderOrigins`、`ValidateLyricsSourceFixedIdentity`、`validPublicV3RevisionURL` 和 v37（`TestPublishLyricsDocumentServesProjectSekaiFandomSource`）。
- 旧版歌词首存之后仍可修改来源、行结构和日文，store 与协作 checkpoint（`validateImmutableDraft`）两条路径都已放开，普通首存也接受受管 Vocaloid Wiki 来源；verified preview/import 仍只用于首存（`TestPublishedLegacyLyricsAcceptProvenanceAndJapaneseEdits`、`TestOrdinaryFirstSaveAcceptsCompleteManagedProvenance`、`TestCheckpointCommitsSavedLegacyJapaneseAndLineStructureEdits`）。
- source-v3 歌曲的 `publish` / `unpublish` 不再返回 `422 unsupported_publication`，改由 `SetSourceV3LyricsWithdrawn` 写入或清除撤下标记。成功时返回 200 和未改动的 rendition 文档；revision 不符返回 409 `revision_conflict`，`current` 为当前文档（`TestSourceV3UnpublishAndPublishToggleTheServedSong`）。
- rendition 编辑器现在会保存独立 Game 面的排版（`TestLyricsRenditionEditorPersistsIndependentGameLayout`）。没有任何译文时只改排版的保存，由 500 改为 `422 translation_required`（`TestLyricsRenditionLayoutOnlySaveWithoutTranslationRequiresTranslation`）。
- 旧版 v1 详情只在修订 URL 符合 v3 规范规则时输出 `attributions`，否则只保留 `attribution`，避免 pjsk.moe 整首拒收（`TestServedLegacyV1DetailAttributionsPassTheMainSiteRules`）。

### 恢复台账接管与导出（第 8 波）

- 恢复导入台账（`lyrics_recovery_import_items`）里的歌曲现在可以用 `PUT /api/editor/v1/lyrics/document` 整曲发布，不论条目带 source 文档还是只有可用性状态。
  - 第一次非 dryRun 的发布在同一事务里、删除恢复 source 文档之前，向 `lyrics_recovery_takeovers` 写一行接管记录：优先取拥有当前 source 文档的台账条目，并逐字保存该文档；没有这样的条目时取最新条目。
  - 台账各行保持不变。dryRun 和被拒绝的发布都不写接管记录。
  - 测试：`TestPublishLyricsDocumentTakesOverRecoveryLedgerSongs`、`TestRecoveryTakeoverSupersedesTheItemOwningTheSourceDocumentOrElseTheNewestItem`、`TestPublishLyricsDocumentRecordsNoRecoveryTakeoverWhenThePublishIsRefused`、`TestV38TakeoversMatchTheirItemAreImmutableAndCascadeWithTheLedger`。
- 接管后的歌曲在编辑器 provenance（`resolveLyricsRenditionEditorProvenance`）、目录（`CatalogMusic` 不再报台账的 `lyricsAvailabilityState`）和公开投影里都按普通编辑器发布的歌曲处理；之后再发布不会写新的接管记录，也没有接口能撤销接管。
- 内容备份：`translation-content/lyrics.json` 新增可选字段 `recoveryTakeovers`，导出、恢复和恢复图校验都已接入，被取代的文档仍按原条目校验 artifact 与 contribution；没有该字段的旧备份照常恢复（`TestRecoveryTakeoverBackupRoundTripsAndStillOpensInTheEditor`、`TestPreTakeoverBackupRestoresAndItsSongsCanBeTakenOver`、`TestRecoveryTakeoverRestoreRejectsTakeoversThatDoNotMatchTheLedger`）。
- 新增 `GET /api/editor/v1/lyrics/document?musicId=N`：编辑即可调用，不需要 producer 头，也不取内容锁。
  - 它把公开站当前提供的详情（v1–v4）转成 PUT 的请求体，`expectedRevision` 已填好；请求格式装不下的内容逐条列在 `warnings` 里。公开站没有该曲时返回 `404 not_found`（`TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest`、`TestPublicLyricsDetailReturnsACopyOfTheServedBytes`）。
  - 内嵌基线包全部 694 首的往返结果见下文第 8b 波（`TestLyricsDocumentExportRoundTripsTheEmbeddedBundle`）。
- PUT 接受页面名含 `?`（写作 `%3F`）的维基链接（`TestParseLyricsDocumentSourceURLAcceptsQuestionMarkTitles`）。
- 旧版 v1 歌曲只凭来源发布（不填署名）时，还要求公开详情能从来源给出 `attributions`，否则返回 `422 incomplete_publication`。此前这类歌曲可以发布，但公开详情既没有 `attribution` 也没有 `attributions`，会被 pjsk.moe 拒收（`TestSourceOnlyLegacyPublicationRequiresAServedSourceAttribution`）。
- 控制台协作保存（`POST /api/editor/v1/lyrics/{musicId}/checkpoint`）改动了 source-v3 文档时立即请求重建（`PublishNow`），不再等去抖重建；没有改动时不触发（`TestLyricsCheckpointOfAChangedSourceV3DocumentPublishesImmediately`）。
- 控制台歌词编辑：
  - 旧版（v1/v2）歌曲在有可保存修改时发布或取消发布，会先保存，保存成功后才发布，按钮为「保存并发布」/「保存并取消发布」。保存失败或修订冲突时发布状态不变，对话框保留并显示原因，冲突时可以「载入服务器版本」；发出发布请求前还会再核对一次共享文档与已保存内容（`tests/lyrics-publication.test.mjs`）。
  - 发布对话框的“先保存”分支、警告文字和发布前检查改看 `saveable`，不再只看本客户端的 `dirty`，协作者留下的未保存修改不会再被当成“已保存草稿”。
  - 其他客户端保存后，本客户端的撤销栈会清空（`retireLocalHistory`）：已被别人保存的修改不再让本客户端保持 dirty，放弃时也不会回退已保存内容；服务端快照之后、广播到达之前做的修改仍算未保存，可以保存。检查点边界改为按撤销栈项截断，广播先于本客户端的保存响应到达时，保存期间的新修改不会被丢掉（`tests/lyrics-document-loader.test.mjs`、`tests/yjs-collaboration-contract.test.mjs`）。撤销历史已被清掉的修改不能再用“放弃”回退，它们作为共享的未保存修改留在房间里。
  - 已撤下的 source-v3 歌曲保存后提示「已保存（当前已撤下，未公开）」，保存按钮为「保存（已撤下）」（`tests/lyrics-document-view.test.mjs`）。是否撤下看目录条目的 `lyricsWithdrawn`，见第 8b 波。

### 导出往返、草稿冲突与恢复修正（第 8b 波）

- 整首文档请求新增四种可选写法，此前合法的请求仍编译成同一份文档：
  - 行的 `segments[]` 携带分段演唱者：各段 `ja` 拼接后必须等于整行 `ja`，每段单独解析，注音不能跨段（`TestLyricsDocumentSegmentsCarryPerSegmentPerformersInTheirOrder`、`TestLyricsDocumentSegmentsAreValidated`）。
  - `game: "only"` 表示只有 Game 的 rendition；全曲只有这类 rendition 时公开为 `game_only`（`TestLyricsDocumentGameOnlyRenditionKeepsTheServedState`）。
  - rendition 的 `translationCredits` 替换文档署名，不论该 rendition 有没有译文；`{}` 表示不署名（`TestLyricsDocumentRenditionCreditsReplaceTheDocumentCredits`）。
  - `ja` 里 `{{`、`}}` 写字面的花括号，此前这两种写法一律被拒（`TestLyricsDocumentMarkupEscapesLiteralBraces`）。
- 行与分段的演唱者按给出的顺序保存，只去掉重复；只有 rendition 的演唱者名单仍按 ID 排序。
- 行长度的报错写明实际上限：`ja` 8192 字节，`zh` 16384 字节（`TestLyricsDocumentLineLimitsNameTheEnforcedSizes`）。
- 尚未发布的旧版草稿不会再被悄悄覆盖。导出的 `expectedRevision` 和 PUT 的冲突比较都取 `conflictRevision`：先取旧版发布、内嵌包、已编辑的 source-v3 译文（revision 大于 1 时才计入）三者中最新的 revision，再与旧版草稿 revision 取较大者。导出新增警告 `unpublished_draft_replaced`。GET 与 PUT 之间有人保存旧版草稿时，PUT 返回 409，详情为 `expectedRevision N does not match the current revision M`（`TestExportLyricsDocumentReportsAnUnpublishedLegacyDraftThatPUTReplaces`、`TestExportLyricsDocumentReportsADraftOverALegacyPublication`）。
- 请求格式现在装得下分段演唱者、只有 Game 的 rendition、演唱者顺序、字面花括号和 rendition 自己的署名，第 8 波为这五项设的导出警告已删去。
- 基线包往返测试改为真实 PUT，发布后再导出一次，要求没有警告且请求不变；只允许 `lyricsDocumentRoundTripPermittedWarnings` 列出的警告，并逐行比较（`TestLyricsDocumentExportRoundTripsTheEmbeddedBundle`）。最终结果：
  - 694 首里 1 首完全相同，642 首只在白名单字段上不同。
  - 50 首还有差异，都有对应警告：Game 投影改为 independent 34 首，来源链接重新编码 18 首。
  - 1 首（795，来源在 `zh.moegirl.org.cn`）被 PUT 拒收，导出时已有 `source_unsupported` 警告。
  - 逐行比较的 48661 行没有差异。
- 内容备份与恢复：
  - 接管记录计入 `lyrics.json` 的清单计数和内容备份的记录总数上限，写入与恢复预检的计数一致（`TestTranslationContentPreflightCountsRecoveryAndRenditionLyricsGraph`）。
  - 已存的 v1 发布按它发布时的规则恢复（有翻译署名，或符合当时的只凭来源发布规则）；第 8 波新增的 `attributions` 要求只用于新发布，这类旧发布照常提供（`TestRestoreKeepsASourceOnlyV1PublicationItsPublishRuleAdmitted`、`TestRestoreStillRejectsAV1PublicationWithoutCreditOrSourceAttribution`）。
- 离线工具：
  - 可以打开 v37、v38 的库：`lyrics-stage` 接受连续的 v18–v38 历史，`lyrics-recovery-import` 与 `lyrics-recovery-public-candidate` 接受 v27–v38，v39 仍被拒绝（`TestLyricsImportRuntimeSchemasAllowReviewedV27ThroughV38Contiguously`、`TestRunRejectsUnreviewedV39RuntimeWithoutCreatingOutput`）。
  - 被接管批次的恢复公开候选（v3、v2 兼容、v4）改用接管记录里逐字保存的文档生成，与接管前相同，备份恢复后也不变。被取代的 legacy v2 条目只带台账里的日文（revision 1，没有译文），因为挂在恢复文档上的编辑器译文已随文档删除（`TestRecoveryPublicCandidatesOfATakenOverBatchKeepTheLedgerContent`、`TestRecoveryPublicCandidatesOfATakenOverLegacyV2ItemCarryOnlyTheLedgerText`）。
- 目录与控制台：
  - `GET /api/catalog/music` 的条目在歌曲带撤下标记时输出 `lyricsWithdrawn: true`，否则省略该字段（`TestCatalogMusicReportsLegacyLyricsWithdrawals`、`TestCatalogMusicReportsSourceV3LyricsWithdrawals`）。
  - 控制台只凭 `lyricsWithdrawn` 判断 source-v3 歌曲是否已撤下。公开镜像没有提供、也没有撤下记录的歌曲单独显示：保存按钮为「保存（尚未公开）」，保存后提示「歌词已保存（此前未公开）」，既不说已撤下，也不承诺保存即公开（`tests/lyrics-publication.test.mjs`、`tests/lyrics-document-view.test.mjs`）。

### 整首文档编辑、接管端点与恢复译文（第 8c 波）

- `expectedRevision` 改为必填。
  - 适用范围：歌曲存有或正在提供任何歌词时，包括旧版草稿或发布、source-v3 文档、恢复台账条目、内嵌包条目和公开详情。
  - 缺少时 PUT 返回 `422 expected_revision_required`，`current.revision` 给出应发送的值；什么都没有的歌可以省略，或发送 `0`。此前缺省时 PUT 不做并发检查，会直接删掉未发布的旧版草稿。
  - GET 的 `404` 也带 `current.revision`，即新建时要发送的值。
  - 测试：`TestPublishLyricsDocumentRequiresExpectedRevisionOnlyWhenSomethingIsStoredOrServed`、`TestPublishLyricsDocumentRevisionsStayAboveBundleAndCheckExpectedRevision`、`TestLyricsDocumentExportRouteReturnsTheServedSongAsARequest`。
- dryRun、PUT 和 takeover 的响应新增 `changes`。
  - 比较对象依次是公开内容、数据库里可编辑的状态、空。
  - 逐项列出来源、rendition、`game`、实际公开的署名，以及各行 `ja`、`ruby`、`segments`、`performers`、`zh`、`stanzaBreakBefore`、`inGame` 的改动和前后值。
  - 最多逐条列 200 条，计数始终完整。
  - 原样提交导出得到 `{"against":"served","changed":false}`；基线包里被 PUT 接受的 693 首都是这样。
  - 测试：`TestLyricsDocumentChangesAreEmptyForAnUnchangedExport`、`TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter`、`TestLyricsDocumentChangesListABoundedNumberOfLines`、`TestLyricsDocumentExportRoundTripsTheEmbeddedBundle`。
- 新增 `POST /api/editor/v1/lyrics/document/takeover`（仅管理员），请求体为 `{musicId, expectedRevision}`。
  - 它把公开站正在提供的歌导出后原样发布，公开内容不变；恢复台账、旧版发布和内嵌包提供的歌都适用。
  - 公开站没有提供这首歌时返回 `404`；其余检查与 PUT 相同，被拒绝时什么都不写。
  - 控制台读取的 source-v3 文档新增 `recoveryLedgerOwned`：台账拥有的歌源层只读，只能改译文；管理员可以点「转为可编辑」调用这个端点。
  - 测试：`TestLyricsDocumentTakeoverRoutePublishesTheServedSongAsADocument`、`TestTakeOverLyricsDocumentMakesAServedLedgerSongAnEditorDocument`、`tests/lyrics-recovery-takeover.test.mjs`。
- GET 新增查询参数 `from=served|database` 和响应字段 `from`。
  - `database` 读数据库里可编辑的 source-v3 文档（已撤下的也读）或旧版草稿。
  - 公开站没有提供这首歌时，缺省的 `served` 自动回落到数据库。
  - 新增警告 `withdrawn_republished`（提交会重新公开已撤下的歌）和 `credit_missing`（没有任何 rendition 带署名，PUT 会拒收）。
  - 测试：`TestExportLyricsDocumentFromDatabaseReadsTheEditableState`、`TestExportLyricsDocumentFromDatabaseReadsALegacyDraft`。
- 导出警告：
  - `unpublished_draft_replaced` 改为按内容判断：旧版草稿与公开内容不同就告警，不论 revision 大小，并建议改用 `from=database`（`TestExportLyricsDocumentReportsADraftThatDiffersFromTheServedSongWhateverItsRevision`）。
  - `served_revision_differs` 的提示写出两种原因：投影还没重建完，或该 revision 的 source-v3 译文没有任何署名，投影永远不会提供它。
- 导出保留多名演唱者分段的边界，只合并相邻、各自最多一名且相同演唱者的分段。此前基线包里 5 首歌（68、299、307、378、637）共 11 行的分段边界会被合并掉（`TestLyricsDocumentExportRoundTripsTheEmbeddedBundle`、`TestExportLyricsDocumentRoundTripsALegacyV1Detail`）。
- 文档级署名只在存在“有 `zh`、又没有自己 `translationCredits`”的 rendition 时必填；每个带 `zh` 的 rendition 都自带署名时可以不填。最后至少要有一个 rendition 带署名（`TestPublishLyricsDocumentNeedsDocumentCreditsOnlyForRenditionsWithoutTheirOwn`）。
- 校验一次返回全部问题（`TestPublishLyricsDocumentReportsEveryIssueInOneBoundedResponse`）。
  - 每条都带位置；markup 问题另带字符下标（按 `ja` markup 字符串计）和出错原文。
  - rendition 级问题不再挡住对其下各行的检查。
  - 最多列 200 条，超出时末尾加一条说明还有多少条未列出。
- v38 的接管记录保存被取代文档的译文。
  - 接管 source-v3 台账文档时，把它的全部 `zh-CN` 译文和译本存进 `localizations_json`，写入前先校验。
  - 内容备份导出和恢复逐字节往返，篡改会被拒收。
  - 被接管条目的恢复公开候选（v3、v2 兼容、v4）与接管前相同，包括译文和译本。
  - 这修正了第 8b 波的说法：只有被取代的 legacy v2 条目才只带台账里的日文。
  - 测试：`TestV38TakeoverLocalizationsBelongToASupersededSourceV3Document`、`TestRecoveryTakeoverKeepsTheSupersededLocalizations`、`TestSupersededRecoveryLocalizationsCaptureTranslationEditions`、`TestRecoveryTakeoverLocalizationsRoundTripThroughBackupsByteForByte`、`TestRecoveryTakeoverRestoreRejectsTamperedLocalizations`、`TestRecoveryPublicCandidatesOfATakenOverTranslatedItemKeepTheLedgerTranslations`、`TestRecoveryPublicCandidatesOfATakenOverItemKeepTheLedgerTranslationEditions`。
- 离线 `lyrics-recovery-import` 在 v38 库上拒绝新批次，只要其中有已被接管的歌。批次必须覆盖整个曲库，所以出现任何一次接管后都不能再导入新批次（`TestNewRecoveryBatchRefusesSongsTakenOverByADocumentPublish`）。
- 控制台：已公开、但没有任何 rendition 署名的 source-v3 歌曲，保存不会公开。
  - 保存按钮为「保存（未署名，暂不公开）」，保存后提示「歌词已保存；还没有署名，暂不公开」。
  - 署名栏上方提示先填署名。
  - 测试：`tests/lyrics-recovery-takeover.test.mjs` 中的 “a served song without any rendition credit is not labelled as published on save”。
- 文档：`contracts/editor-api/README.md` 改写成面向 agent 的编辑配方，内容包括：
  - 改错字、注音读音与拆分、拆行合行、段落空行、分段演唱者、Game 三种模式、转义、署名、换来源，每个配方都附 dryRun 的 `changes`；
  - `expectedRevision` 的必填与比较规则、`from=database` 和 takeover。

  `ROLLBACK_RUNBOOK.md` 的冒烟第 6 步补上会被 PUT 拒收的条件（含没有署名、没有 `zh`），并加入不写入的 takeover 检查。`PRODUCTION_CONTRACT.md` 与 `README.md` 同步了 takeover 路由、`changes`、`localizations_json`、各离线命令的 schema 范围和离线导入守卫。

### 多译本歌曲、dryRun 与行摘要（第 8d 波）

- 整曲文档路由支持有多个 `zh-CN` 译本的歌。
  - 此前：GET 丢掉非默认译本，并给出警告 `translation_editions_dropped`。带译本状态的歌（迁移 v32 的歌曲 682、控制台建过译本的歌）在 PUT 和 takeover 时报 `FOREIGN KEY constraint failed (1811)`，返回 500，dryRun 却报告成功。
  - 请求新增文档级 `translationEditions`（第一项是默认译本）、行级 `zhEditions` 和 rendition 级 `editionCredits`。省略、写 `[]` 或写 `[{"key":"main","label":"默认译本"}]` 时，与原来一样只有隐式译本 `main`，不写译本行。
  - 带 `translationEditions` 时要求 1–16 项，`key` 匹配 `^[a-z0-9][a-z0-9._-]{0,127}$`、不重复、含 `main`，`label` 去掉首尾空白后为 1–256 字节。`zhEditions` 和 `editionCredits` 只能用已声明的非默认译本；“至少一行 `zh`、至少一个署名”只要求默认译本。issue 新增 `edition` 字段（`TestPublishLyricsDocumentLocatesEveryTranslationEditionIssue`）。
  - GET 导出全部译本，`from=served`（v4 详情）和 `from=database` 都一样。原样提交导出时 `changes` 为空，各译本的内容和署名不变；第一次提交会把 `sourceTabPaths` 改写为版本名、给 `provenance` 补上注音一项，之后再提交，公开详情除 `revision`、`updatedAt` 外逐字节不变。
  - 只有一个改过名的 `main` 译本的歌，公开站提供 v3，里面没有译本名；数据库状态与公开内容一致时，GET 导出和 `changes` 的比较基准从数据库补上 `translationEditions`，原样提交不再丢掉译本名（`TestLyricsDocumentExportKeepsTheLabelOfARenamedOnlyEdition`）。
  - PUT 按控制台编辑译本的方式写入：译本行、译本状态、各译本的本地化与各行，以及与默认译本一致的 legacy 镜像。发布后控制台可以继续改名、设默认、按译本保存。`replaceTx` 先删译本状态，再删 source 文档。
  - 歌曲有多个译本时，PUT 响应里的 `document` 是 v4 详情，否则仍是 v3。
  - `translation_editions_dropped` 已从服务端、控制台和文档中删除。
  - 测试：`TestPublishLyricsDocumentReplacesASongWithTranslationEditionState`、`TestPublishLyricsDocumentKeepsTheTranslationEditionsOfSong682`、`TestTakeOverLyricsDocumentKeepsTheEditionsOfARecoveryLedgerSong`、`TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail`、`TestLyricsDocumentRubyFixKeepsEveryTranslationEdition`、`TestLyricsDocumentLineSplitRealignsEveryTranslationEdition`、`TestLyricsDocumentAddsRenamesAndSwitchesTheDefaultTranslationEdition`、`TestConsoleEditionEditingWorksAfterALyricsDocumentPublish`、`TestExportLyricsDocumentCarriesEveryTranslationEditionOfAV4Detail`。
- dryRun 在事务里执行与 PUT 完全相同的写入（含接管记录和 `replaceTx`），然后回滚；不重建投影，也不重置协作房间。PUT 会遇到的失败，包括数据库层的失败，dryRun 同样返回（`TestPublishLyricsDocumentDryRunFailsWhereThePublishFails`）。
- `changes` 也比较译本：
  - 文档级 `translationEditions` 给出前后的完整列表；
  - rendition 级 `editionCredits` 按译本列出署名变化；
  - 行的 `fields` 可以含 `zhEditions`，并附各译本的前后译文。
- `changes` 里新增和删除的行摘要带上整行结构：`stanzaBreakBefore`、解析后的 `performerIds`、`segments`（多段，或分段演唱者与整行不同时）、`inGame`（`cut` rendition 的 Full 行）、`zhEditions`。此前歌曲 328、502 里两行相同的相邻行之间挪动段落空行时，只显示一删一增两条文字相同的行，看不出挪了什么（`TestLyricsDocumentChangeSummariesShowTheStructureOfAddedAndRemovedLines`）。
- 离线 `lyrics-recovery-import` 还会拒绝这样的新批次：批次里有某首歌的条目，而这首歌的 source 文档归编辑器所有。
  - 归编辑器所有的文档有两类：`manifest_batch_sha256` 全零的文档（整曲文档路由、takeover 和迁移 v32 写入的都是这种），以及内嵌编辑器 seed 写入的文档。
  - 报错写明 music ID，错误包装 `ErrLyricsRecoveryImportConflict`；重放已导入的批次不受影响（`refuseRecoveryItemsForEditorOwnedSongs`，`TestNewRecoveryBatchRefusesSongsWithEditorOwnedSourceDocuments`）。
  - 运维后果：批次必须覆盖整个曲库，所以只要有一首歌用整曲文档路由发布过或被接管过，就不能再导入新的恢复批次。v32 写入过歌曲 682 的数据库（目录里有 682 就会写入）从一开始就是这样。
- 控制台：
  - 导出警告的中文说明补上 `withdrawn_republished` 和 `credit_missing`，删掉 `translation_editions_dropped`（“every export warning the server emits reads in plain Chinese”）。
  - 公开站没有提供、或已撤下的 source-v3 歌曲，「转为可编辑」按钮禁用，并写明原因和出路（“the conversion is disabled with the way forward while the site does not serve the song”、“when the site serves nothing the dialog gives the way forward and lists no differences”）。
  - 说明条写明：转换后，日文和注音要通过整曲文档接口修改（“the notice says Japanese and ruby are edited through the whole-song document route after conversion”）。
  - 确认对话框只在没有会改变公开页面的警告时，才写“公开页面的内容保持不变”；有这类警告时分组列出（“the dialog promises an unchanged public page only when no warning changes it”）。
  - 因为还没有加载 producer proof 而被拒绝的转换，可以在校对完成后重试（“a takeover refused for a missing producer proof can be retried once the proof is loaded again”）。
  - `api.ts` 补上 `translationEditions`、`zhEditions`、`editionCredits` 和 `changes` 的完整类型；issue 带 `edition`，报错文字写出是哪个译本（“an issue about a translation edition names the edition and the field in Chinese”）；PUT 与 takeover 响应的 `document` 类型改为 v3 或 v4。
  - 以上测试都在 `tests/lyrics-recovery-takeover.test.mjs`。
- 文档：
  - `contracts/editor-api/README.md` 新增配方 12（多译本的歌：改日文或注音时保留每个译本、拆行时重新对齐 `zhEditions`、增加、改名、切换默认、删除译本，以及每条校验错误的原文），补上新的请求字段、`changes` 字段、行摘要字段和 dryRun 的新语义。
  - 改正配对规则的说法：完全相同的行作锚点，锚点之间的行按位置配成 changed，只有多出来的行才算增删。
  - 删掉 `translation_editions_dropped`。
  - `PRODUCTION_CONTRACT.md`、`README.md`、`ROLLBACK_RUNBOOK.md` 同步了这些改动和新的离线守卫。`ROLLBACK_RUNBOOK.md` 新增冒烟第 7 步：导出歌曲 682，原样 dryRun，要求 `changes` 为 `{"against":"served","changed":false}`。

### 补注音接口与分段报错

- 新增 `POST /api/editor/v1/lyrics/document/ruby`（任何已登录用户；不读写歌曲，不要门禁头，不取内容锁）。请求体 `{"lines": [ja markup, …]}`，1–2000 条，每条单行、不超过 8192 字节。
  - 给还没有注音的汉字按词补上 Kagome / IPADIC 词典读音，例如 `今日も見上げる空` → `{今日|きょう}も{見上|みあ}げる{空|そら}`；已写的注音和 `{{`、`}}` 原样保留。
  - 每条返回 `ja`、新加的注音 `suggested`（带它在返回 `ja` 里的字符下标）和 `problems`（这个 `ja` 按 PUT 校验仍会报的问题，`[]` 表示可以直接提交）。和已写注音重叠的词、词典读不出的字留在 `problems` 里。花括号没闭合等写法问题时原样返回。
  - 读音是词典的常用读法，歌词里的特殊读法（如「運命|さだめ」）要按来源核对后改掉。
  - 用途：旧格式（v1）的歌没有注音，导出时带 `ruby_not_served` 警告，此前 agent 要给整首歌的每个汉字手写读音。
  - `lyricssource.SuggestRubySpans` 逐词给出读音，读不出的词保留为纯文本；来源导入用的 `GenerateDeterministicRubySpans` 行为不变（抽出了共用的 `tokenRubySpans`）。
  - 测试：`TestSuggestLyricsDocumentRubyFillsOnlyKanjiWithoutRuby`、`TestLyricsDocumentRubyRouteSuggestsReadingsForAnySignedInUser`；手册新增 4.5.2。
- 分段拼接不等于整行 `ja` 的 issue 现在给出两边从分歧处开始的文字（最多 16 个字符），例如 `…they first differ at character 8: the line has "の{歌|うた}", the segments give "{歌|うた}"`。此前只给字符下标；改了有分段的行的注音却忘了改分段时（本地实测歌曲 68 第 0 行），要自己去数字符。

### 发布后马上导出不再拿到旧内容

- 公开文件在发布后异步重建。此前在重建完成前 `GET` 导出，拿到的是被替换的旧歌词，`expectedRevision` 却是新的；agent 在这份导出上改下一处再 `PUT`，会悄悄把上一次发布改回去（本地实测：`PUT` 后立即导出，得到旧内容和 `served_revision_differs`）。
- 现在 `GET`、`PUT`、takeover 在读公开内容前先调用 `filesvc.Service.AwaitPublished`：有排队的重建时立即执行（跳过防抖窗口），最多等 10 秒。等待发生在取任何锁之前；超时照常返回，旧内容仍带 `served_revision_differs`。
- 测试：`TestLyricsDocumentExportWaitsForAPendingPublication`（去掉等待时失败：导出 revision 2 的内容，`expectedRevision` 为 3）、`TestAwaitPublishedPublishesADebouncedRequestAtOnce`。

### 控制台保存 Game 面与非默认译本

- 精确投影（`cut`/`same`）的 Game 面：控制台改 Full 行的译文时，按 `relation.lineIds` 找到对应的 Game 行一起改。此前按行下标对齐，Game 行少于 Full 行时改到了错误的行，保存返回 `422 invalid_game_projection`（`exact-projection Game rows follow a Full translation edit through relation.lineIds`）。
- 协作房间会跨重启保留，旧控制台留下的错位 Game 行仍在房间里。checkpoint 在比较前先用 `store.ProjectExactGameTranslations` 把 Game 行重新投影到 Full 行，这类房间可以直接保存（`TestLyricsCheckpointProjectsStaleExactProjectionGameRows`）。
- 在非默认译本上连续保存两次，第二次返回 `422 source_drift`。原因是 checkpoint 把刚保存的非默认译本的指纹记为房间的 `authority_sha256`，而漂移检查读的是默认译本。现在提交事务内读取默认译本（`store.DefaultLyricsRenditionDocumentTx`）来记指纹（`TestLyricsCheckpointsOfANonDefaultEditionKeepTheRoomUsable`：同一连接上两次保存都返回 200，默认译本不变；去掉修复时第二次返回 422）。
- 一个房间同一时刻只装一个译本，保存写的是房间里的那个。此前用 `?edition=alt` 打开时，如果房间里装的是 `main`，页面标签显示 alt，保存却写进 main。现在标签和地址栏跟随房间的 `translationEditionKey`（`lyrics-document-loader.test.mjs`）。
- 旧格式（legacy）歌词在首存后，checkpoint 不再把来源信息、行结构和日文当作不可改的字段；它们和普通保存一样交给 store 校验。
- 目录侧栏：source-v3 歌曲显示「数据库：source-v3 文档」（撤下时加「（已撤下）」），不再显示「草稿」；旧格式歌曲撤下后显示「草稿（已撤下）」。`GET /api/catalog/music` 的条目为 source-v3 歌曲输出 `lyricsSourceV3: true`，否则省略该字段（`TestCatalogMusicReportsSourceV3LyricsWithdrawals`、`the catalog labels a source-v3 song as a document and shows withdrawals`）。

### gachaInfo 分类

- 新分类 `gachaInfo` 排在 `gacha` 之后，字段为 `summary`、`bubbleText`、`description`，由 `extractGachaInfo` 从 `gachas.json` 的 `gachaInformation` 按 gacha id 配对 JP 与 CN。键保留原文的换行和首尾空白，不做 trim。CN 同步步骤表 `cnSyncSteps` 已加入该分类，公开文件 `translation/gachaInfo.json`、`gachaInfo.full.json` 及 `v2` 镜像自动生成（`TestExtractGachaInfoPairsByIDWithExactKeys`、`TestCNSyncStepsCoverEverySupportedCategory`、`TestUpdateEntryAcceptsLongMultilineGachaInfoDescription`）。
- AI 翻译：`xmlUnescape` 一次解码五个预定义实体和数字字符引用；prompt 要求保留换行；`llmBatches` 除条数上限外，还按 `maxLLMBatchTextBytes`（8 KiB）限制每批日文字节数（`TestManualAITranslateGachaInfoKeepsLongMultilineText`、`TestLLMBatchesBoundCountAndBytes`）。
- `extractMysekai` 拉取 JP 分类或标签失败时，现在会返回错误，不再静默产出空字段（`TestExtractMysekaiPropagatesJPGenreAndTagFetchErrors`）。

### 控制台

- 歌词编辑：
  - 日文修改能写回行数据（`lineWithEditablePatch`）；为新演唱者配色时读 `side.version.label`，不再抛 TypeError。
  - 旧版歌词首存后仍可调整行结构和日文；source-v3 和精确投影的 Game 面保持只读。
  - source-v3 的保存按钮改为「保存并公开」。技术卡片收进默认折叠的「技术详情」，会锁定写入的协作状态仍显示在折叠区外。
- 协作：
  - dirty 只跟随本客户端的 Yjs 本地修改（`hasLocalChanges`）；只要共享文档与基线不同就可以保存（`lyricsDocumentSaveable`），所以别人留下的未保存修改也能由其他协作者保存。
  - 编辑器卸载后才返回的加载，不会再启动协作连接。
- 词条编辑：
  - 输入法组字期间按键不再触发保存或导航（`translationTextareaAction`）。
  - `gachaInfo` 使用独立的分类名和字段名（`fieldLabel`）；多行原文和译文保留换行，列表中最多显示 6 行；编辑框旁提示换行键。
  - 长原文（超过 4 行或 240 个字符，例如卡池说明）：宽屏（≥1024px）时原文和编辑框左右并排，原文在自己的区域内滚动，编辑框最多 10 行；窄屏时原文区最高 45vh、自带滚动。此前编辑区固定为 420px 高，卡池说明的原文就占满了它，“保存并下一条”被挤到下方列表后面，点不到（Orca 实测：按钮中心点落在列表的来源列上）。
  - 详情链接：`gachaInfo` 指向主站 `/gacha/<id>/`，活动剧情改为 `/story/event/<id>/`。

### 备份与配置

- 内容备份导出和恢复接受父证据行缺失的 artifact 证据引用（内嵌 seed 和文档路由写入的 artifact 都是这种情况）。此前含 seed 插入歌曲的数据库导出会报 `has no exact parent evidence`（`TestSeededDatabaseContentBackupRoundTripsWithoutParentEvidence`、`TestRoutePublishedSongInSeededDatabaseContentBackupRoundTrips`）。
- 生产 Git 备份自 2026-08-11 起每次都失败，报 `lyrics source artifact evidence 1/fixed-88fb…/0 has no exact parent evidence`，原因就是上一条：旧版导出给歌曲 1 的 seed artifact 补链接时不检查 list-of-songs 父证据行是否存在。本地演练用生产同款旧版（`bd95715`）加 8/11 备份、seed 和生产的歌词文档，在没有该父证据行的库上复现了完全相同的报错。另外，导出补链接只接受 id 和摘要都一致的父证据，其余引用保持未链接；恢复校验同样只在备份里有同摘要父证据时才要求链接；已有链接指向的父证据不一致时仍然拒绝（`TestContentBackupParentEvidenceWithOtherBytes`、`TestContentBackupKeepsReferenceUnlinkedWhenSharedParentHasOtherBytes`）。
- 恢复接受没有分段行的歌词行。生产库里有 36 首歌的数据库文档（例如歌曲 50）存着没有 segment 行的歌词行，本地演练中任何含这些文档的备份都恢复失败，报 `lyrics document 50 violates segment_mismatch`。恢复校验把这种行当作一整段、无演唱者的行来检查；有分段的行仍要求各段拼起来等于日文（`TestRestoreAcceptsStoredLyricsLinesWithoutSegments`）。
- 早于 `gachaInfo` 的 S3/Git 备份和旧 seed 可以恢复：`hasLegacyRestoreLayout`、importer 的 `loadCategory` 和 `cmd/migrate` 接受两个文件都缺失的情况，只缺其中一个仍报错（`TestS3RestoreAcceptsArchivePredatingRestoreOptionalCategory`、`TestS3RestoreRejectsHalfPresentRestoreOptionalCategory`、`TestImportExportPredatingGachaInfoRestoresItEmpty`、`TestSeedMigrationAcceptsSeedPredatingGachaInfo`）。
- 未加密 Git 备份在内容未变时不提交、不推送，记为成功，不再因 “nothing to commit” 失败（`TestUnencryptedGitBackupSucceedsWithoutCommitWhenContentIsUnchanged`）。
- 设置写入（`SetMany`）和环境变量种子（`SetManyIfAbsent`）都保存去掉首尾空白后的值，secret 也一样；纯空白值等于清空并回落默认值（`TestSetManyStoresTrimmedValues`、`TestSetManyIfAbsentStoresTrimmedValues`）。
- translator 测试全部改用本地测试源（`configureLocalSources`），不会再回落到公共镜像去联网。
- `TestLargeTranslationContentPreflightHonorsCancellation` 改为确定性：用第 1000 次检查起报告取消的 context，代替 1 ms 后取消的定时器。原写法在全量测试负载下，取消常常晚于扫描结束，偶发失败。

### 文档

- 新增 `contracts/editor-api/README.md`（面向 agent 的写入指南），含 GET 导出与 `warnings` 表、基线包往返结果、`segments` / `game: "only"` / `translationCredits` / 花括号转义的写法与示例、推荐流程（GET → 改 zh → PUT 带导出的 `expectedRevision`）、恢复台账接管，以及 PUT 的检查顺序与错误表。
- `ROLLBACK_RUNBOOK.md` 写明生产由 Zeabur 从 `main` 源码构建，并加入部署检查清单：v35–v38 只能前进、首次部署走 v34→v38 并生成 `moesekai.db.pre-migration-v35.bak`、快照与磁盘、环境变量范围、部署后冒烟（GET 导出后 PUT dryRun）与备份检查。
- `PRODUCTION_CONTRACT.md` 同步了门禁、文档路由与导出、恢复台账接管、目录的 `lyricsWithdrawn`、`gachaInfo`、v37、v38、离线工具的 schema 上限和备份规则；`README.md` 也写明了离线工具的 schema 上限。

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
