# 外部 SQL 数据源：提取、管理与只读问数

状态：设计已确认；P1a 已实现（见第 10 节）　范围：MySQL、PostgreSQL（第一期）

## 0. 目标与整体流程

让用户把业务数据库接入 WeKnora，实现三件事：

1. **提取**：从数据库的结构与数据中提取知识，先生成原始文档入库，再编译为 wiki。提取要尽量充分，包括显式的外键、字段画像，以及隐含的关联关系（通过集合相似度与 LLM 深化）。
2. **管理**：数据源作为知识库的一种数据源，复用现有的配置、凭证、校验、同步、日志与权限。
3. **问数**：Agent 基于 wiki 理解表结构与关系，生成 SQL 在外部库上只读执行，在对话中给出结论、结果表格与分析。

```
数据源配置（/api/v1/datasource，type=mysql|postgres）
   │  Validate / ListResources（schema → 表，懒加载选择）
   ▼
同步（ProcessSync → 流式 FetchStream，每表一个 FetchedItem）
   │
   │  P1 提取（只读，不写库）
   │   L1 结构：表、列、类型、主键、索引、声明外键
   │   L2 字段画像：按列的样本特征（范围、TopK、非空率、大字段长度与最近时间）
   │   L3 隐含关系：MinHash/LSH 候选 + 证据数字（包含度、Jaccard）
   │   L4 LLM 深化：基于 L1–L3 的证据做语义判断，结果经结构校验与缓存
   ▼
原始文档（每表一篇 Markdown，ExternalID = 表标识）
   │  P1d：关系同时写入知识图谱（若知识库开启图谱）
   │  知识库开启 wiki → post-process 自动 EnqueueWikiIngest
   ▼
P2 wiki 编译：只读取原始文档（不直接访问数据库）→ 表页、字段含义、表间关系
   │
   ▼
P3 问数：Agent 只从 wiki 中获取表与字段信息 → 生成 SQL
   │  external_db_query（只读事务、限时、限行、表白名单、审计）
   ▼
对话输出：结论 + 结果表格 + 分析 + 所用 SQL + 局限说明
```

数据流的约束：P2 的原料只有原始文档；P3 的表结构与关系知识只来自 wiki（强制执行的限制见第 6.4 节，当前为提示词约束）。

与 `database_query` 的区别：`database_query` 查询 WeKnora 自身的库（知识库、文档、分片），新工具 `external_db_query` 只查询用户接入的外部库，二者不混用、不重名。

## 1. 已确认的决定

| 议题 | 决定 | 依据 |
|---|---|---|
| 支持的数据库 | MySQL、PostgreSQL | 用户确认 |
| 提取产物 | 先存原始文档（每表一篇），再由 wiki 编译整理；P2 只读原始文档 | 用户确认 |
| 字段画像策略 | **按列**提取有判别意义的特征，不按行取前 N 行。ID 与时间给范围；低基数文本/数字/布尔给 TopK；高基数文本给去重数；大字段（text/blob/json）给非空率、平均长度、最近非空时间；敏感列只给非空率 | 用户确认 |
| 画像开关 | 统计特征与取值特征**默认都开启**；敏感列按规则自动脱敏 | 用户确认 |
| 隐含关系 | 深度挖掘：MinHash/LSH 候选 + LLM 深化；允许借助 LLM 做更深入的提取 | 用户确认 |
| 关系存储 | 同时写入原始文档与知识图谱 | 用户确认 |
| LLM 所在阶段 | 属于 P1（提取阶段的深度分析），P2 不接触数据库 | 用户确认 |
| P3 知识来源 | 只从 P2 编译的 wiki 中取数据，不直接访问原始文档 | 用户确认 |
| P3 强制限制 | 暂不做强制（仅提示词约束），后续再评估 | 用户确认 |
| SQL 安全 | 严格只读：单条 SELECT/WITH，只读事务，强制限行限时，表白名单 | 用户确认 |
| 报告形式 | 直接在对话中以 Markdown 输出 | 用户确认 |
| MySQL SQL 解析器 | 原定 `vitess.io/vitess/go/vt/sqlparser`；因 v0.22.0 / v0.23.0 无法在 Go 1.26 上编译、v0.24.5 要求 go 1.26.9 而放弃，改为 `github.com/pingcap/tidb/pkg/parser`（与当前 Go 版本兼容）。PostgreSQL 使用 `pg_query_go` | 用户确认（解析器选择）；版本变更后待确认 |
| 分期 | P1 拆为 P1a–P1d 依次实现，每段都有验收 | 用户确认 |

## 2. 现有实现的复用点

| 能力 | 现有位置 | 本设计的用法 |
|---|---|---|
| 连接器接口 | `internal/datasource/connector.go`：`Connector`、`StreamingConnector`、`FullStreamingConnector` | 实现 `FullStreamingConnector`，获得“全量重取 + 删除对账”能力 |
| 连接器注册 | `internal/container/container.go` 的 `initConnectorRegistry` | 注册 `mysql`、`postgres` |
| 元数据与类型列表 | `ConnectorMetadataRegistry`、`ListAvailableConnectors` | `AuthType=password`，前端类型列表自动出现 |
| 凭证与非敏感配置 | `DataSourceConfig.Credentials`（加密、不回显）、`Settings`（非敏感） | `user`、`password` 进 Credentials；连接地址与画像参数进 Settings |
| 创建校验 | `CreateDataSource` → `validateDataSourceConfig` → `connector.Validate` | `Validate` 建连并 `SELECT 1`（只读事务） |
| 无状态校验 | `POST /datasource/validate-credentials` | 新增可选 `settings` 字段，SQL 的“测试连接”需要 host/database（P1a 已加） |
| 同步与对账 | `processSyncStreaming`、`streamSyncHandler`（Emit / Checkpoint）、`applyFetchedItem` | 每表 `Emit` 一个文档；游标记录每表指纹；未变化的表跳过 |
| 单项失败 | `applyFetchedItem` 对 `Metadata["error"]` 的处理（计入 `Failed`，同步日志为部分成功） | 单表失败以错误条目上报；流式路径**不**使用 `PartialFetchError`（流式路径未处理该错误，会整体失败） |
| 入库 | `ingestItem`：按 `external_id` 替换旧文档；`CreateKnowledgeFromFile` | `ExternalID` 为 `["schema","table"]` 的 JSON，`FileName = schema.table.md` |
| wiki 编译 | `knowledge_post_process.go` 的 `willSpawnWiki` 分支 → `enqueueWikiIngestTrigger` | 不新增触发逻辑；数据源所在知识库开启 wiki 即可 |
| 知识图谱 | `internal/application/service/graph.go`（`GraphBuilder`、`Entity`、`Relationship`） | P1d 写入关系实体与边（需先核对写入接口） |
| 集合相似度 | `internal/searchutil/textutil.go` 的 `Jaccard` | L3 的候选验证 |
| Agent 工具注册 | `definitions.go`、`agent_service.go` 的 `registerTools`、`WithSearchTargets` | 同第 7.3 节清单 |
| 知识库范围 | `SearchTargets`、`authorizeKnowledgeInSearchTargets`、`searchTargetScope` | 外部库的访问范围按知识库判定 |
| 审计 | `recordKBActivity`，`AuditAction*` 常量 | 每次外部查询记一条 |
| 工具守护 | `CanRunConcurrently`（默认屏障）、`tool_policy_coverage_test.go` | 新工具必须通过这些约束 |
| SSRF | `utils.SSRFSafeDialContext`、`utils.RegisterMySQLSSRFDialer` / `MySQLSSRFNetwork` | 两种数据库的连接都经由安全拨号 |

## 3. 数据源管理与提取（P1）

### 3.1 连接器包

目录：`internal/datasource/connector/sqldb/`（P1a 已实现，见第 10 节）

| 文件 | 职责 |
|---|---|
| `connector.go` | 导出 `NewMySQLConnector`、`NewPostgresConnector`；Validate、ListResources、同步（FetchStream / FetchFullStream） |
| `config.go` | 设置解析与默认值、类型收敛（JSON 数字、字符串、列表） |
| `dialect.go` | 方言接口与只读查询公共逻辑（`readRows` 在 READ ONLY 事务中执行） |
| `mysql.go` | information_schema 元数据；`ORDER BY 主键 DESC LIMIT n` 采样；SSRF 拨号 |
| `postgres.go` | pg_catalog 元数据；`TABLESAMPLE SYSTEM … REPEATABLE` 采样；`default_transaction_read_only=on` |
| `profile.go` | 列分类、画像查询构造、按列聚合（纯函数，便于测试） |
| `render.go` | 表 → Markdown（确定性输出） |
| `cursor.go` | 游标结构与绑定校验（同一数据源不得静默切换数据库） |
| `ids.go` | 资源标识（JSON 数组编码）与选择集 |
| `sketch.go` | 列的 bottom-k MinHash 草图、确定性哈希、重叠估计（L3） |
| `relations.go` | 候选生成、判定、推断关系按表分组（L3） |

元数据（`ConnectorMetadataRegistry`，已实现）：

| type | Name | AuthType | Capabilities | Priority |
|---|---|---|---|---|
| `mysql` | MySQL | `password` | `incremental`、`hierarchical`、`deletion_sync` | 14 |
| `postgres` | PostgreSQL | `password` | `incremental`、`hierarchical`、`deletion_sync` | 15 |

`incremental` 指按表指纹的增量：未变化的表不重新入库。它不依赖数据库中的游标字段。

### 3.2 资源标识与选择

- 资源标识是 JSON 数组，避免名称中出现任何字符时产生歧义：schema 为 `["shop"]`，表为 `["shop","orders"]`。
- 文档的 `ExternalID` 与表的资源标识使用同一编码，选择、文档身份与游标键保持一致。
- 选中 schema 即包含其下所有表；选中表只包含自身。选择集之外的已有文档在同步时被删除（删除对账）。
- MySQL 中 schema 即数据库本身，只暴露配置的 `database`。

### 3.3 配置

非敏感（`Settings`）：

| 字段 | 说明 | 默认 |
|---|---|---|
| `host`、`port`、`database` | 连接地址与数据库 | port 按引擎：3306 / 5432 |
| `tls_mode` | `disable` / `require` / `verify-full` | `require` |
| `profile_enabled` | 是否采集字段画像（L2） | `true` |
| `profile_values` | 是否采集取值特征（范围、TopK）；关闭后只保留统计特征 | `true` |
| `topk` | 每列 TopK 取值数（1–50） | 10 |
| `profile_sample_rows` | 每表样本行数（100–100000） | 10000 |
| `masked_columns` | 额外敏感列名，逗号分隔，大小写不敏感 | 空 |
| `max_tables` | 单次同步表数上限（1–5000） | 200 |
| `query_timeout_seconds` | 单条查询超时（1–600） | 30 |

敏感（`Credentials`，加密存储，不回显）：`user`、`password`（密码可为空）。

P1a 的连接参数为一次同步一个连接池（最多 2 个连接），每个连接都经由 SSRF 安全拨号。

### 3.4 Validate 与 ListResources

- `Validate`：解析配置 → 建连（失败返回可读错误，包含目标地址但不含密码）→ 只读事务 `SELECT 1`；MySQL 同时确认数据库存在。
- `ListResources`：`parentID == ""` 返回 schema（`HasChildren=true`）；`parentID` 为 schema 时返回其表（`Description` 为表注释，`Metadata.row_estimate` 为估算行数）；表是叶子。
- `ResolveResourceAncestors`：对表返回其 schema 的标识。

### 3.5 同步流程

```
对每个选中的表（按 schema、表名排序）：
  1. describe：列、类型、可空、默认值、注释；主键；索引；声明外键；估算行数
  2. profile（profile_enabled 时）：一次有界采样，按列聚合（见 3.6）
     - 画像失败时降级：文档只含结构，并注明“列画像暂不可用”，不使整表失败
  3. render：生成 Markdown；计算 SHA-256 指纹
  4. 指纹与游标一致且非全量同步 → 跳过
  5. 否则 Emit 文档，并更新游标指纹
  6. 每 20 张变化的表 Checkpoint 一次（断点续传）
然后：
  7. 游标中存在、但本次未出现在选择集或数据库中的表 → Emit 删除条目，并从游标移除
```

错误与边界：

- describe 失败（如表被锁、权限不足）→ 发出错误条目（`describe_failed`，固定文案，不含驱动错误细节），同步计为部分成功，**保留旧指纹**，下次重试；该表不会被误删。
- 超过 `max_tables` → 按字母序截断，发出错误条目 `max_tables_exceeded`，被跳过的表视为“仍存在”，旧文档不删除。
- 游标绑定：游标记录 `target = engine://host:port/database`，切换到其他数据库会被拒绝（`ErrInvalidConfig`），避免误删旧库的文档。
- 游标 `schema_version` 变化（渲染格式升级）时整体重建一次。

### 3.6 字段画像（L2）

目标：用一次有界采样得到“有判别意义”的列特征，而不是原始行。

| 列角色 | 判定 | 输出特征 |
|---|---|---|
| 敏感（`roleMasked`） | 列名含 password、passwd、pwd、secret、token、phone、mobile、id_card、idcard、ssn、bank、card_no、email，或在 `masked_columns` 中 | 仅非空率；不输出任何取值 |
| 标识（`roleID`） | 名为 `id`、以 `_id` 结尾，或为单列主键/唯一索引，且类型为数字或文本 | 数字标识给范围，不给平均值；文本标识给平均长度；低基数（≤50）且取值重复时给 TopK |
| 时间（`roleTime`） | 日期、时间戳类型 | 范围（最小、最大） |
| 数值（`roleNumeric`） | 整数、小数、浮点 | 范围、平均值；低基数（≤500）且取值重复时给 TopK |
| 布尔（`roleBool`） | bool | TopK |
| 文本（`roleText`） | char、varchar、enum、set、uuid 等 | 低基数（≤500）且取值重复时给 TopK；否则给去重数与平均长度（前 256 字符） |

TopK 的“取值重复”判定：样本内去重数 × 2 ≤ 非空行数，即平均每个取值至少出现两次。接近唯一的列（如业务单号）只报告去重数，不列出噪声取值。TopK 取值会进入知识库，因此含业务敏感内容的列应放入 `masked_columns`。
| 大字段（`roleLarge`） | text、blob、json、geometry 等 | 非空率、平均长度（服务端计算）、最近非空时间（取时间列） |
| 其他 | 无法识别的类型 | 仅非空率 |

采样方式：

- **PostgreSQL**：统计信息可用时 `TABLESAMPLE SYSTEM(pct) REPEATABLE(seed)`，`pct` 约为样本数的 3 倍除以估算行数，`seed` 由表名决定，保证未变化的表得到相同样本；统计信息缺失时退化为物理顺序前部，报告中注明。
- **MySQL**：有主键时 `ORDER BY 主键 DESC LIMIT n`（InnoDB 按主键聚簇，代价可控），取到的是“最新的 n 行”，对历史数据有偏，报告中注明；无主键时退化为无序前 n 行。
- 所有查询在只读事务中执行；取值在 Go 侧聚合，SQL 只返回聚合所需的最少列（大字段只返回 `IS NULL` 与长度）。

输出示例（渲染结果的一部分）：

```markdown
## 列画像（基于样本）

- 样本行数：10000，采样方式：latest by primary key
- 数值与时间给出范围，低基数取值给出 TopK 占比，大字段仅给出非空率、平均长度与最近非空时间，敏感列只给出非空率

| 字段 | 角色 | 非空率 | 取值与范围 | 其他特征 |
|---|---|---|---|---|
| id | 标识 | 100.0% | 范围 1 ~ 9998 | 样本内去重 10000；平均值 5000 |
| status | 文本 | 100.0% | TopK: paid (62.0%); done (21.5%); pending (9.0%) | 样本内去重 5 |
| body | 大字段 | 91.2% | （大字段，不展示取值） | 平均长度 412；最近非空时间 2026-10-14 09:12:03 |
| buyer_email | 敏感(已脱敏) | 99.8% | （敏感列，不展示取值） | — |
```

报告中的 `估算行数` 四舍五入到两位有效数字，使统计小幅波动不改变文档字节，从而不触发无谓的重新入库。

### 3.7 文档结构

`ContentType=text/markdown`，`FileName=schema.table.md`，`Metadata`：`channel`（`mysql` 或 `postgres`）、`source_type`（引擎）、`db_schema`、`db_table`。

文档包含：标题与数据源信息；字段结构表；索引；声明的外键；推断关系（L3，启用画像时）；列画像（可选）；数据质量提示（样本中几乎全空、常量列）。不包含时间戳，不包含样本行原文。

### 3.8 隐含关系（L3，P1b）

目标：发现数据库中**未声明**的关联，例如 `orders.buyer_id` 实际指向 `users.id` 却没有外键。

前提：关系发现需要所有选中表的样本，因此同步分三个阶段——先读取全部表（结构与样本），再做关系发现，最后渲染并只重发指纹变化的文档。关系发现依赖画像开关（`profile_enabled`），与 `profile_values` 无关：关系证据只含数字，不含取值。

方法（实现于 `sketch.go`、`relations.go`）：

1. 候选列：标识列、数值列、文本列（不含敏感列与大字段），且样本中去重取值不少于 20 个。数值与文本分属两类，互不比较。
2. 每列对去重取值的哈希保留最小的 512 个（bottom-k MinHash，即 KMV 草图）。哈希为 FNV-1a 加 splitmix 收尾，确定性，未变化的数据得到相同草图。去重取值不少于 512 时草图为估计，少于 512 时精确。
3. 候选生成：共享草图中的任一哈希的两列互为候选（单行 LSH 分桶）。出现在超过 32 列中的哈希视为通用值（0、空串等），不用于连接。候选列总数超过 2000 时跳过关系发现，并在文档中说明。
4. 对候选对估计交集与并集，得到包含度 `|A∩B| / |A|`、`|A∩B| / |B|` 与 Jaccard。
5. 判定（保守阈值，只输出证据数字）：
   - 被引用端 B 近似唯一（去重数 / 非空行 ≥ 0.9）且去重数 ≥ 100，且 A 的取值在 B 中的包含度 ≥ 0.9 → `inferred_fk`（A 引用 B）；
   - 两端互相包含（等值的唯一集合）→ `overlap`（方向不确定）；
   - Jaccard ≥ 0.5 且两端去重数均 ≥ 50 → `overlap`；
   - 已在数据库中声明的外键（任一方向）不再重复报告。
6. 结果写入两端的文档：源表写“推断关联”，被引用表写“推断关联（被关联一侧）”，取值重叠两端都写。每条都注明证据数字（包含度或 Jaccard、两端去重数），文末说明结论基于样本估算。

阈值的理由：被引用端只有几十个取值时（如 35 个编码），源端的小整数几乎必然落在其中，包含度失去意义，因此要求被引用端至少 100 个去重取值。取值重叠在小集合（如租户码，20–30 个取值）上多为巧合，因此要求 50 个以上。

已知局限（需在文档与后续 L4 中体现）：

- 样本为“按主键降序取前 n 行”（MySQL）或 TABLESAMPLE（PostgreSQL），两端样本可能不覆盖相同的取值范围，包含度会被低估，导致漏报；
- 审计字段（`create_by`、`update_by`）之间的重叠普遍存在且 Jaccard 接近 1，它们确实引用同一批用户，但信息量低，L4 应对此做语义过滤；
- 数值型小范围编码（如 1–100 的状态码）若恰好落在某个唯一整数列的范围内，可能被误判为外键，因此要求被引用端至少 100 个取值，并需要 L4 复核。

真实库结果（`lowcode_pplatform`，79 张表，全量同步）：推断关联约 50 条，被引用关系与之对应；`parent_group_id`、`cust_company_id`、`apply_id` 等未声明的关联均被找到。

### 3.9 LLM 深化（L4，P1c）

目标：对 L3 的候选与 L1 的命名、注释做语义判断，补充 L3 无法给出的信息（如“订单状态”的含义、业务上的关联方向）。

约束：

- 输入只包含结构、注释、画像特征与 L3 证据数字，**不包含原始取值**；敏感列不进入提示词。
- 输出必须是结构化 JSON，经校验：引用的表与列必须存在于 L1，关系类型必须在白名单内；校验失败的条目丢弃并记日志。
- 结果按缓存键缓存：键为 `sha256(版本号, 模型标识, 语言, 系统提示词, 用户消息)`，即模型实际看到的全部内容。键不变时不调用模型；缓存保存在游标的 `analysis` 字段中，表离开选择集时删除。
- 校验（`parseSemantics`）：只接受合法 JSON 对象；列名必须存在于 L1；关系的 `column` 与 `peer` 必须是本表与 L3 给出的候选，`verdict` 必须在 `likely` / `unlikely` / `uncertain` 中；超长文本截断；不合格的条目丢弃并计数。整个回复无法解析时，该表结果整体作废，文档标注“未通过校验”。
- 校验通过的判定只附加在 L3 关系行上（“语义判定”），不改变 L3 的证据数字，也不新增关系。
- 模型选择：由 `DataSourceService.ProcessSync` 通过上下文注入（`datasource.WithAnalysis`），优先使用数据源的 `ProfileConfig.ModelID`，其次使用知识库的摘要模型；取不到时记日志并继续，不注入则 L4 整体关闭。连接器不直接依赖模型服务。
- 提示词模板：`config/prompt_templates/db_table_analysis.yaml`（新增，ID `default_db_table_analysis`，可在会话配置中替换）。模板内容与代码中的默认值保持一致；模板中的 `{{language}}` 由同步请求上下文中的语言决定（`types.LanguageNameFromContext`），未指定时为英文。
- Token 预算：每次同步默认上限 500k tokens，由 `llm_token_budget` 设置（上限 5M，`0` 表示不调用模型）。单表输出上限 8192 tokens；输出被截断（`finish_reason=length` / `max_tokens`）的表标注为截断，不采用其结果。调用前按 `（系统提示词 + 用户消息）字节数 / 3` 估算，调用后以模型返回的 `TotalTokens` 为准。超出后剩余表只保留 L1–L3 结果，并在文档中写明“未执行：超出预算”。该默认值仍待确认（见第 11 节）。
- 熔断：连续 3 次调用失败后，本次同步不再调用模型，剩余表标注“已停止调用模型”。失败结果不进入缓存，下次同步会重试。
- 模型不可用、超时或返回异常：降级为仅 L1–L3，不使同步失败；只有取消同步会使其失败。
- 开关：`llm_analysis`（默认开启）。关闭时输出与 P1b 完全一致。
- 调用使用 `Temperature 0.1`、`MaxTokens 2048`、关闭思考模式，调用目的记为 `db_table_analysis` 以便审计。
- 文档中的“语义解读”小节标注“模型辅助，基于结构与统计，未使用取值”，列出每列的含义（标注是否来自注释还是推断）以及关系判定。

### 3.10 知识图谱写入（P1d）

- 每条已确认关系（声明外键、推断关联，以及 L4 通过校验的关系）写入知识图谱：表为实体，关系为边，边上携带证据类型与来源文档的 `chunk`/`knowledge` 引用。
- 写入发生在提取阶段，与文档入库相互独立；图谱写入失败不影响文档入库，记日志并在同步日志中标注。
- 写入接口需先核对现有 `GraphBuilder` 的入口（当前为基于分片的 LLM 抽取，P1d 前确认是否需要新的写入方法）。

已核对（P1d 前置检查）：

- `types.GraphBuilder` 只有内存中的 `BuildGraph(chunks)`，由 LLM 从分片中抽取实体与关系，没有“写入一条已确认关系”的入口，且不在任何服务中被调用。
- 实际持久化路径是 `interfaces.RetrieveGraphRepository.AddGraph(ctx, NameSpace{KnowledgeBase, Knowledge}, []*GraphData)`，由 chunk 级抽取任务（`extract.go`）在每个分片上调用，节点携带 `Chunks`（分片 ID）。
- 同步时数据源文档尚未分片，分片 ID 不存在；因此由连接器直接写图谱时，节点与边无法携带分片引用，检索时无法回溯到文档内容。
- 如果知识库开启了抽取（`ExtractConfig.Enabled`），每篇渲染后的数据源文档都会经过上述 chunk 级抽取，其中“声明外键”与“推断关系”小节的文字本身就会被 LLM 抽取为图谱边。因此确定性写入是对该路径的补充，而不是唯一来源。

决定（P1d 结论）：不新增确定性写入。图谱边由现有 chunk 级抽取从数据源文档中获得，文档中的“声明外键”与“推断关系”小节即为其证据。这样图谱节点始终带有可回溯的分片引用。代价是边的质量取决于 LLM 对文档的抽取，且只有开启抽取的知识库才有图谱；若后续需要确定性的表间关系，再单独评估。
- 知识库未开启图谱时跳过图谱写入，关系只保留在文档中。

## 4. 与 wiki 的衔接（P2）

- P2 的原料只有原始文档（第 3 节产物）。wiki 编译任务不得直接访问数据库。
- 编译提示（新增）：对 `channel=mysql|postgres` 的文档生成表页，包含用途、主键、关键字段、关系（声明与推断，区分证据强度）、枚举字段的取值含义（来自 TopK 与注释）。
- 数据源所在知识库未开启 wiki 时，`CreateDataSource` 返回提示（不阻断）。

已实现（P2）：

- 编译提示：`mapOneDocument` 在文本充分性检查之后、抽取之前，对 `channel` 为 `mysql` / `postgres` 的文档在正文前加入提示（`wiki_ingest_channel_hint.go`）。提示要求按表页组织，声明外键与推断关系分开写，推断关系引用给出的证据数字，取值重叠无方向，枚举含义只能依据注释或取值分布，不得臆造；“语义解读”小节视为模型辅助阅读而非数据库事实。其他渠道的文档不受影响。
- 未开启 wiki 的提示：MySQL / PostgreSQL 数据源创建时，若知识库未开启 wiki，响应中带 `notice: "wiki_disabled"`（不落库，`types.DataSourceNoticeWikiDisabled`）。编辑器据此弹出警告，文案随六种语言提供（`datasource.sql.wikiDisabledNotice`）。
- 测试：提示仅对两种渠道生效，数据源类型判断（`wiki_ingest_channel_hint_test.go`）。`CreateDataSource` 的 `notice` 只做了代码审查，未有单元测试（夹具依赖多）；需在页面上确认。
- 尚未验证：提示对真实模型生成的表页质量的影响，需在真实模型端到端中检查。

## 5. 只读问数（P3）

### 5.1 工具：`external_db_query`

输入：

- `knowledge_base_id`：Agent 检索范围内、包含一个 `mysql` / `postgres` 数据源的知识库 ID（来自 `runtime_context`，模型可直接获得）。若该知识库只有一个此类数据源则自动使用；若有多个，返回候选 `datasource_id` 列表，由模型重试时传入 `datasource_id`。
- `datasource_id`（可选）：数据源 ID，必须属于 Agent 检索范围（`SearchTargets`）内的知识库，且为 `mysql` / `postgres` 类型。
- `sql`：单条 SELECT 或 WITH 语句。

输出：`Output` 为 Markdown 表格；`Data` 包含 `columns`、`rows`、`row_count`、`truncated`、`display_type=external_db_query`。

### 5.2 执行流程

```
1. 权限：数据源所属知识库 ∈ SearchTargets，否则拒绝
2. 静态检查：
   a. 解析失败（MySQL：TiDB parser；PostgreSQL：pg_query_go）→ 拒绝
   b. 多语句 → 拒绝
   c. 根语句必须是 SELECT 或 WITH…SELECT；拒绝 SELECT…INTO、FOR UPDATE/SHARE、锁定读、SET、CALL 及带副作用的函数（黑名单仅作为补充）
   d. 引用的表必须全部是该数据源已提取的表（CTE 别名不算表）；仅允许 wiki 中出现过的表（第 6.4 节）
3. 建连（SSRF 安全拨号，超时）
4. 只读事务：PostgreSQL `BEGIN READ ONLY` + `statement_timeout`；MySQL `START TRANSACTION READ ONLY` + `max_execution_time`
5. 流式读取，达到 max_result_rows（默认 200）或 max_result_bytes（默认 512 KiB）即取消，不改写 SQL，不用 LIMIT 包装
6. 回滚只读事务，关闭连接
7. 审计：租户、知识库、数据源、用户、会话、SQL 摘要、耗时、行数、截断、错误
```

### 5.3 为什么不靠 LIMIT 包装与关键字黑名单

- 包装成 `SELECT * FROM (...) LIMIT n` 会破坏部分方言语法（如 MySQL 子查询别名要求），且无法限制流式读取量。流式计数并取消更可靠。
- 关键字黑名单会误伤合法列名与字符串。静态检查基于语法树，数据库层只读事务是最终保证。

### 5.4 Agent 工作流（提示词）

在 `config/prompt_templates/agent_system_prompt.yaml` 的数据库问数段落中约定：

1. 先用 `wiki_search` 找相关表页，再用 `wiki_read_page` 读取字段含义与表关系。
2. 只能使用 wiki 中出现过的表和字段，不得臆造；原始文档不作为取数依据。
3. 生成 SQL 后调用 `external_db_query`；出错时根据报错修正，最多 3 次。
4. 最终回答包含：结论、结果表格（超过 20 行给摘要）、分析、所用 SQL、数据局限（样本范围、估算行数、截断情况、数据时间）。

### 5.5 并发策略

`external_db_query` 不加入 `CanRunConcurrently`，保持屏障语义。

## 6. 安全设计

### 6.1 威胁与防护

| 威胁 | 防护 |
|---|---|
| SSRF：通过数据源地址探测内网 | MySQL 使用 `RegisterMySQLSSRFDialer` 与 `MySQLSSRFNetwork`；PostgreSQL 的 `DialFunc` 调用 `SSRFSafeDialContext`。内网数据库需运维在 `SSRF_WHITELIST_EXTRA` 中放行 |
| 写入或破坏数据 | 只读事务 + 语法树检查 + PostgreSQL 会话级 `default_transaction_read_only=on` + 建议只读账号 |
| 越权访问其他知识库的数据源 | `SearchTargets` 按知识库判定，与检索、wiki 一致 |
| 访问未提取的表 | 语法树提取表名，必须在已提取表集合内 |
| 大查询拖垮服务 | 语句超时、行数与字节上限、流式取消 |
| 凭证泄露 | 加密存储；API 不回显；日志不打印 DSN 与密码；同步日志不含驱动错误细节 |
| 敏感数据进入 LLM | 敏感列不采集取值；L4 输入不含原始取值；问数结果受脱敏配置约束（P3 确认） |
| 审计缺失 | 每次查询（含失败）记录审计 |

### 6.2 P1a 已落实

- 所有查询在只读事务中执行；PostgreSQL 会话额外设置只读参数。
- 连接经由 SSRF 安全拨号；配置校验拒绝缺失的 host、database、user 与非法的 `tls_mode`。
- 同步过程中的驱动错误只进入服务端日志，同步日志使用固定文案。连接测试（Validate）返回的错误会带上驱动信息以便排查，但不包含密码。
- 敏感列在 SQL 层即只返回空值标记，不返回取值。

### 6.3 P3 已落实

- SQL 静态检查：MySQL 用 TiDB parser，PostgreSQL 用 pg_query_go；只允许单条 SELECT 或 WITH…SELECT，拒绝写入、锁定读、`INTO`、`SET`、`CALL` 与阻塞/读服务器文件类函数。
- 表白名单：引用的表必须属于该数据源最近一次同步提取的表（CTE 别名不算表）。
- 结果上限：200 行、512 KiB，流式读取，达到上限即取消；超时 60 秒。
- 只读事务：MySQL `START TRANSACTION READ ONLY`，PostgreSQL 只读事务与会话级 `default_transaction_read_only=on`。
- 审计：每次查询（含失败）写入审计日志，记录 SQL 的 SHA-256 摘要而非原文。
- 未解决：问数结果未脱敏（见第 11 节第 6 项）。

### 6.4 P3 知识来源限制

P3 的表结构与关系知识只来自 wiki。当前为提示词约束；强制限制（例如工具层只允许访问 wiki 中出现过的表）暂不实现，后续评估。

## 7. 改动清单（按模块）

### 7.1 P1a 已完成

| 文件 | 改动 |
|---|---|
| `internal/types/datasource.go` | `ConnectorTypeMySQL`、`ConnectorTypePostgres` |
| `internal/types/knowledge.go` | `ChannelMySQL`、`ChannelPostgres` |
| `internal/datasource/connector.go` | 两项元数据 |
| `internal/datasource/connector/sqldb/**` | 连接器、方言、画像、渲染、游标、标识与测试（新增） |
| `internal/container/container.go` | 注册两个连接器 |
| `internal/handler/datasource.go`、`internal/application/service/datasource_service.go`、`internal/types/interfaces/datasource.go` | `validate-credentials` 接受可选 `settings` |
| `frontend/src/api/datasource/index.ts` | `validateCredentials(type, credentials, settings?)` |
| `frontend/src/views/knowledge/settings/DataSourceEditorDialog.vue` | 两个类型的表单定义，连接设置与画像设置区块，步骤校验，测试连接传递 settings |
| `frontend/src/i18n/locales/*.ts`（6 个） | `datasource.sql.*`、连接器名称与描述、同步错误文案 |

图标：暂使用默认图标（未新增二进制资源）。

### 7.2 P1b / P1c / P1d 预计改动

| 阶段 | 文件 | 改动 |
|---|---|---|
| P1b | `internal/datasource/connector/sqldb/relations.go`、`sketch.go`（已新增） | bottom-k MinHash、候选生成、证据计算、推断关系渲染 |
| P1c | `internal/datasource/connector/sqldb/llm.go`（新增）、`config/prompt_templates/db_table_analysis.yaml`（新增） | 提示词、结构校验、缓存、预算、降级 |
| P1d | `internal/application/service/`（图谱写入，接口待核对） | 关系写入图谱，失败不影响入库 |

### 7.3 P2 / P3 预计改动

P2：`wiki` 编译提示（新增一条 `db_table` 类文档的编译规则）；创建数据源时的 wiki 提示。

P3 工具登记清单（每一项都要做；已于 P3 实现时完成，除非注明）：

| 位置 | 改动 |
|---|---|
| `internal/agent/tools/definitions.go` | `ToolExternalDBQuery = "external_db_query"`；`AvailableToolDefinitions` 增加条目 |
| `internal/agent/tools/external_db_query.go` | 工具实现（新增） |
| `internal/agent/tools/execution_policy.go` | 不加入 `CanRunConcurrently` |
| `internal/agent/tools/capabilities.go` | 能力要求（存在已提取的外部数据源）；与 `frontend/src/utils/tool-capabilities.ts` 保持一致 |
| `internal/modelcontext/tool_policy.go` | 声明策略（`tool_policy_coverage_test.go` 强制要求） |
| `internal/application/service/agent_service.go` | `registerTools` 增加分支，使用 `WithSearchTargets` |
| `internal/agent/act.go`、`internal/agent/grounding_prompt.go`、`internal/agent/observe.go` | 展示名、数据类工具列表、工具分类 |
| `config/prompt_templates/agent_system_prompt.yaml` | 问数流程说明（`hybrid_rag_wiki_agent`、`wiki_researcher`）；新增模板 `external_db_analyst` |
| `config/agent_type_presets.yaml`、`config/builtin_agents.yaml` | 新增预设类型 `external-db-qa` 与内置 Agent `builtin-external-db-qa`，参照数据分析预设；`kb_filter` 要求知识库启用 wiki |
| `frontend/src/views/chat/components/tool-results/ExternalDBQuery.vue`（新增）、`ToolResultRenderer.vue`、`frontend/src/types/tool-results.ts` | 结果渲染与 display_type 注册（已完成） |
| `go.mod` | 新增 `github.com/pingcap/tidb/pkg/parser`（MySQL 语法检查）与 `github.com/pganalyze/pg_query_go/v6`（PostgreSQL）；`x/mod`、`x/telemetry`、`x/text`、`x/tools`、`go-semver` 为其依赖图的最低要求版本；更新 `THIRD_PARTY_NOTICES.md` 与 `licenses/` |
| 审计 | `internal/types/audit_log.go` 增加审计动作常量 |

## 8. 数据模型与迁移

- `data_sources.type` 为字符串，`config` 为 JSON。P1a 不需要迁移，游标与指纹保存在现有 `last_sync_cursor` 中。
- 外部查询审计复用 `recordKBActivity`，不新建表。
- 若后续需要保存问数历史，再新增 versioned 迁移，并同时提供 up/down 与 SQLite 兼容。

## 9. 测试策略

### 9.1 单元测试（默认运行，无外部依赖）

P1a 已实现（`internal/datasource/connector/sqldb`，共 27 个测试函数，全部通过）：

- 资源标识：编码往返、非法输入、选择集覆盖规则；
- 配置：默认值、类型收敛、非法 `tls_mode`、凭证规范化、PostgreSQL DSN 转义；
- 列分类：类型归一化、角色判定、敏感列与唯一索引规则；
- 画像：范围、TopK（确定性排序）、大字段最近时间、敏感列不泄露、`profile_values` 关闭；
- 渲染：确定性、表格转义、TopK 文本、行数分桶、降级与质量提示；
- 同步语义（假方言）：首次全量、未变化跳过、变化仅重发、离开选择集删除、表被删除、强制全量、描述失败保留旧指纹、`max_tables` 溢出保留、游标绑定拒绝、必须选择资源。

P1b 已实现（`sketch_test.go`、`relations_test.go`）：

- 草图：精确重叠、估计包含度（与真值误差 ≤15%）、不相交集合、确定性与有界性、唯一率；
- 关系判定：未声明外键被找到、已声明外键不重复、低基数与不相交列不参与、数值与文本不比较、取值重叠方向不确定、超上限跳过；
- 渲染：两端都有小节、“未发现”与“已跳过”的区分；
- 同步（假方言携带样本行）：文档中出现双向关系、第二次同步不重发、关闭画像时不输出关系。

P1c 已实现（`llm_test.go`，使用假模型 `fakeChat`，共 9 个测试函数）：

- 判定写入：合法判定附加到关系行，缓存命中时不调用模型；
- 提示词不含取值，敏感列不出现在输入中；
- 不合格输出（非 JSON、无对象、非法列名或 verdict）被丢弃或整表作废；
- 预算耗尽后停止调用并标注原因；
- 连续失败触发熔断，同步仍成功；
- 关闭设置或无模型时 L4 整体关闭；
- 解析只保留经核对的事实（`parseSemantics`）；
- 缓存键随模型或提示词变化而变化；
- 模型回复中的 JSON 提取。

P3 已实现：
- `sqlcheck_test.go`（5 个测试函数）：MySQL 与 PostgreSQL 的合法 SELECT / WITH 通过；写入、锁定读、`INTO`、多语句、不安全函数被拒；游标键解析。
- `external_db_query_test.go`（工具层，5 个测试函数）：数据源不在 Agent 范围内时拒绝且不执行；数据源不存在；参数缺失；Markdown 转义（管道符与换行）与 `Data` 字段；截断提示；服务错误原样返回。
- `datasource_external_query_test.go`（服务层）：非数据库类型被拒绝且错误包装 `ErrInvalidConfig`；SQL 摘要稳定且不含原文。
- `integration_mysql_test.go::TestMySQLIntegrationRunQuery`（需环境变量）：真实 MySQL 上只读 SELECT 成功、行数上限截断、`DELETE` / `UPDATE` / `FOR UPDATE` 被拒、未提取表被拒。测试库为 MySQL 5.7 系列，因此不使用递归 CTE。
- `modelcontext` 策略覆盖测试（`tool_policy_coverage_test.go`）通过：`external_db_query` 为不解码 SQL、结果原样输出的策略。

P1d 与 P2 的单元测试在各阶段补充（图谱写入等）。

### 9.2 集成测试（需环境变量，缺省跳过）

- `WEKNORA_EXTDB_TEST_MYSQL_DSN`、`WEKNORA_EXTDB_TEST_PG_DSN`：真实库的 Validate、同步（含画像）、只读执行、写操作被拒。
- MySQL 真实库验证（已完成）：测试库 `lowcode_pplatform`，共 79 张基础表。Validate、列出 schema、同步（`max_tables` 设为 80 覆盖全部表）生成 79 篇文档且无描述或画像失败；第二次同步重发 0 篇；只读探测的写入被 MySQL 以 READ ONLY 拒绝。
- 真实库验证发现并修复的问题：BIGINT 主键的平均值无意义（已移除）；数字标识被误标“平均长度”（仅文本标识才计算长度）；接近唯一的文本列输出噪声 TopK（已加重复判定）；MySQL `TABLE_ROWS` 以 uint64 返回导致估算行数与数值范围缺失（已修复并加回归测试）。
- PostgreSQL 的真实 SQL（pg_catalog、TABLESAMPLE）**尚未在真实库上验证**。
- P3 的 `RunQuery` 在 MySQL 真实库上已通过上述集成测试；PostgreSQL 同样未验证。

### 9.3 前端

- `DataSourceEditorDialog.test.ts`：既有 18 项通过；SQL 表单用例待补充。
- `npm run check-i18n`：14 项通过（6 个语言文件键集合一致）。
- `vue-tsc --build`：通过。

### 9.4 端到端（手工）

本机安装 MySQL / PostgreSQL（不使用 Docker），在 Lite 模式下完成“建数据源 → 同步 → 编译 wiki → 对话问数”。

## 10. 分期、状态与验收

| 阶段 | 交付 | 状态 | 验收标准 |
|---|---|---|---|
| P1a 结构与画像 | 连接器（L1、L2）、渲染、游标、注册、表单、i18n、单元测试 | 代码与单元测试完成；MySQL 真实库验证通过；PostgreSQL 真实库验证待做 | `go build` 与相关 `go test` 通过；前端类型检查与 i18n 审计通过；真实库上同步得到正确文档，删除表后文档被清理 |
| P1b 隐含关系 | bottom-k MinHash、候选生成、证据计算、推断关系渲染 | 代码与单元测试完成；MySQL 真实库已验证（约 50 条推断关联） | 在带有未声明外键的测试库上，推断关系被找到，证据数字正确；未声明外键被找到；已声明外键不重复；第二次同步不重发 |
| P1c LLM 深化 | 提示词、结构校验、缓存、预算、降级 | 代码与单元测试完成（假模型）；真实模型端到端待做 | 模型不可用时同步仍成功；校验失败条目被丢弃；缓存命中时不调用模型；超预算后剩余表降级 |
| P1d 图谱写入 | 关系写入图谱 | 已决定：不新增确定性写入，由现有 chunk 级抽取覆盖（见 3.10） | 开启抽取的知识库中，表间关系文本经分片抽取进入图谱（待真实模型验证） |
| P2 wiki 编译 | 表页编译提示、未开启 wiki 的提示 | 代码完成（单元测试通过）；未做真实模型验证 | 同步后 wiki 生成表页，包含字段说明与表间关系；编译过程不访问数据库 |
| P3 只读问数 | `external_db_query`、sqlcheck、审计、Agent 工作流 | 代码与单元测试完成；MySQL 真实库只读执行测试通过；内置预设已新增；PostgreSQL 与页面端到端待做（服务已按新代码重启，待页面触发） | 写操作与未知表被拒；超限被截断；审计可查；回答包含 SQL 与局限说明 |
| P4 体验完善 | 结果渲染、可选增量（`updated_at`）、文档 | 结果渲染完成（`ExternalDBQuery.vue`）；文档与页面端到端待做 | 端到端流程在 Lite 模式下可演示 |

每个阶段结束都需要：相关单元测试、`go build ./...`、前端类型检查与 i18n 审计，并更新 `website-docs`。

## 11. 待确认事项

1. **LLM 每次同步的 token 预算**：默认 500k tokens（`llm_token_budget` 可调），超出后剩余表降级为 L1–L3。2026-10-16 经你确认采用 500k。
2. **P3 知识来源的强制限制**：当前为提示词约束，强制限制暂不实现。
3. **MySQL 画像的时间偏差**：`ORDER BY 主键 DESC` 取到的是最新行，对历史数据有偏。是否接受，或改用 `TABLESAMPLE` 等近似随机的方式（需评估 InnoDB 的代价）。
4. **PostgreSQL 验证环境**：MySQL 已用测试库验证；PostgreSQL 尚无可用测试库，需要提供连接信息（通过环境变量传入，不写入仓库）。
5. **内网数据库**：如测试库在内网，需要运维在 `SSRF_WHITELIST_EXTRA` 中加入对应主机。
6. **问数结果脱敏（已决定：不脱敏）**：`external_db_query` 返回所选列的原始取值，问数 SQL 可以直接选取敏感列。决定依赖数据库账号的只读权限控制，并在用户文档中提醒配置最小权限账号。
7. **内置预设（已决定：新增）**：参照数据分析预设新增预设类型 `external-db-qa` 与内置 Agent `builtin-external-db-qa`，工具为 `wiki_search`、`wiki_read_page`、`external_db_query`。
