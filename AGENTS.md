# AGENTS.md — tgfile AI Agent 工程规范

本文件定义 AI Agent 在 tgfile 项目中进行代码修改、测试和审查时必须遵循的规范。

## 0. 总则

- 不允许主动降低代码质量要求，包括放宽 lint、跳过失败测试、删除测试、缩小检查范围，
  或使用 `//nolint` 等方式掩盖可以修复的问题。
- 修改必须优先保证存量 S3 GET/HEAD、文件直链读取和 SQLite 数据兼容性。
- 已发布内容只允许在 S3 DeleteObject/DeleteObjects/S3 覆盖或 WebDAV DELETE、
  COPY/MOVE 覆盖移除最后一个 Mapping 引用后，通过持久化删除状态机处理。
- 未发布 Multipart 暂存内容只允许在 Abort、同 PartNumber 覆盖、Complete 未选择、
  upload 过期或上传失败补偿时进入持久化删除状态机。
- 读取、List、HEAD、PROPFIND、启动、audit、migration、无删除或覆盖的 Mapping 操作和
  历史无删除引用的数据不得触发后端删除。
- 后端删除成功也必须保留 File、Part 和删除状态记录；其他数据清理必须有审计结果、
  明确范围、备份和可验证的迁移/回滚方案。
- 工作区可能包含用户尚未提交的修改。不得覆盖、丢弃或重置无关改动。

## 1. 项目概述

tgfile 是一个 Go 文件服务，将文件切块后存储到可插拔 BlockIO 后端。当前生产核心链路：

- S3 PUT 上传；
- S3 GET/HEAD/Range 读取；
- `/file/download/:key` 文件直链；
- SQLite 保存文件、分片和路径映射元数据；
- Telegram 是主要 BlockIO 后端。

WebDAV、备份和静态目录仍存在，但不是当前主要生产链路。

## 2. 文档规范

- `docs/` 只保存需要长期维护的当前设计，包括系统架构、模块边界、核心流程、功能设计、
  数据与存储模型、稳定 API 语义、兼容性不变量和重要设计决策。
- `docs/` 描述“系统现在如何工作、为什么这样设计、边界是什么”，不得写成修改历史。
  架构、稳定功能语义或数据兼容边界改变时，必须同步更新对应文档。
- `docs/` 禁止保存易变或一次性信息，包括代码 review 结果、缺陷清单、修复方案、lint
  清理记录、测试执行结果、任务状态、临时结论、发布日期、一次性迁移/部署/回滚步骤、
  CI 单次运行结果以及生产主机、目录或凭据。
- 临时设计、review 草稿、排查记录和实施中的中间文档放在 `td/`；PR/Issue 记录、Release
  Notes 和运维执行记录应由对应协作或发布系统保存，不为留痕而复制到 `docs/`。
- `td/` 中的设计或实施方案在对应代码、数据变更和既定验证全部完成后，必须在同一次
  收尾中晋级为正式文档：优先按稳定主题合并到现有 `docs/`，只有无法归入现有主题时才
  创建新的编号文档。
- 晋级后的文档必须能够脱离原 `td/`、任务对话、PR 和 Issue 独立说明当前设计，完整保留
  架构关系、模块职责、协议语义、数据模型、状态机、兼容边界和仍然有效的设计决策；
  不得要求读者回看临时方案才能理解或维护当前实现。
- 晋级时必须删除任务状态、日期、工期估算、实施顺序、待办清单、测试用例数量、单次
  测试结果、临时环境、上线记录以及一次性迁移/部署/回滚步骤。可以保留长期有效的系统
  不变量、验证维度和失败语义，但不得保留某次执行的数量或结论。
- 正式文档覆盖已实施内容后，应删除对应的已完成 `td/` 文档；如果同一文档仍包含未实施
  范围，应把未实施部分拆成新的 `td/` 文档，并只保留待实施内容。纯 review、排查或
  验收记录不整体晋级，只提取其中仍然成立的系统事实。
- `docs/` 文件名使用 `{两位序号}-{稳定主题}.md`，例如 `docs/01-architecture.md`。
  正式文档之间仅引用 `docs/` 下的稳定路径，禁止引用 `td/` 或临时任务编号。
- 使用、配置和开发入口写入 `README.md`；Agent 工作、测试和交付规则写入 `AGENTS.md`；
  `docs/` 不重复维护这些说明。

## 3. 构建与检查

项目使用 Go 1.25.12。常用命令：

```bash
make build                  # 构建 ./cmd
make test                   # 运行全部测试
make test-race              # 运行全部竞态测试
make install-golangci-lint  # 安装固定版本到 ./bin/
make lint                   # 运行 golangci-lint
make check                  # 格式、vet、test、race、lint 全量门禁
```

完成代码修改后必须保证 `make check` 通过。

测试要求：

- 数据库、缓存和文件必须使用 `t.TempDir()`，不得使用固定 `/tmp/*.db`。
- 自动化测试不得连接生产配置、生产数据库、Telegram 或外部 S3 服务。
- HTTP 测试使用 `httptest`；允许 loopback，不依赖预先运行的本地服务。
- 每个修复至少覆盖正常、异常和关键边界路径。
- 新增数据迁移时，测试必须验证 dry-run、前置条件、提交后状态和备份恢复路径。

## 4. Lint 规范

- 使用 golangci-lint v2，配置文件为 `.golangci.yml`，版本由 Makefile 固定。
- 所有源码和测试必须零 issue 通过 `make lint`。
- 除非用户明确要求，不得修改 `.golangci.yml` 来绕过现有告警。
- 禁止通过 `//nolint`、降低复杂度阈值、排除文件或关闭 linter 代替真实修复。
- 注册型 `init()` 只允许出现在 BlockIO 实现和现有框架初始化位置；新增注册应优先显式组装。
- 错误必须保留原因链；需要分类的错误使用 `errors.Is`/`errors.As` 和 `%w`。

## 5. 代码与数据边界

- `entity/` 和 `server/model/` 是数据模型层，不得依赖业务实现包。
- `cmd/` 是组装入口，其他包不得反向依赖 `cmd`。
- `filemgr` 负责文件与路径语义；handler 不得绕过它直接修改业务表。
- `audit` 和 `check-key` 子命令不得初始化 Telegram、HTTP 服务或缓存。
- 只读维护命令必须用 SQLite 只读模式，不能调用会建表或迁移 schema 的初始化逻辑。
- S3 PUT 和 CopyObject 遵循标准覆盖语义，`If-Match`/`If-None-Match` 条件必须在最终
  SQLite 事务中再次判断；被替换内容只有在最后一个 Mapping 引用消失后才能进入删除队列。
- WebDAV DELETE 以及 COPY/MOVE 覆盖必须在同一个 SQLite 事务中递归处理 Mapping、
  S3 Metadata、最后引用判断和 durable outbox；成功响应不等待 Telegram 网络删除。
- Multipart Complete 只能原子发布 layout v2 Composite File，不得重新上传或移动
  Telegram Part；Abort、过期和未选择 Part 不得绕过 durable outbox。
- 默认上传根目录固定为 `/defaults`；运行时代码不得增加历史路径 fallback。
- 外部直链 key、file_id、Telegram FileKey、分片顺序和存量 MD5 不得静默改写。
- 业务 schema DDL 只能放在根目录 `migrations/`，文件名使用 `NNNN_name.sql`；Go 代码中
  只允许保留连接级 PRAGMA 和 migration 账本自身的基础设施 SQL。
- 已发布或可能已被执行的 migration 禁止修改、删除、重命名或换序；schema 调整必须追加
  更高版本，并保持空库初始化和存量库升级结果一致。
- 需要兼容已知历史 schema 时，将完整 SQL 画像放入 `migrations/legacy/` 并精确匹配；
  不得通过忽略列约束、默认值、索引或未知表来扩大通用兼容范围。
- migration 必须可在单事务内执行。无法识别的旧 schema 或 schema 漂移必须安全失败，
  不得自动重建数据库、删除业务表或跳过 checksum 校验。
- `tg_file_part_delete_state_tab` 是 Telegram 删除的 durable outbox；任何代码不得绕过
  `live -> pending -> deleting -> terminal` 状态机直接删除消息或丢弃删除引用。

## 6. 安全与日志

- 日志禁止输出 Bot Token、用户密码、Authorization、完整 Telegram FileKey 或完整下载 key。
- 新增 HTTP Client 必须设置合理的连接、TLS、响应头和整体超时，并传播 context。
- 上传类非幂等操作不得自动重试，除非能证明不会产生重复后端对象。
- 文件路径和外部 key 必须在切片、拼接或访问存储前完成严格校验。
- 缓存只保存可重建副本；缓存异常不得删除 SQLite、Telegram 或 localfile 原始数据。

## 7. Review 与交付

代码修改完成后执行以下闭环：

```text
实现 -> lint/test -> 代码 review -> 修复 -> 再次 lint/test
```

退出条件：

- 没有 P0（崩溃、数据损坏、安全漏洞）；
- 没有 P1（功能错误、结果不正确、兼容性破坏）；
- 没有 P2（未处理边界、明显竞态或资源泄漏）；
- `make check` 全部通过；
- 正式文档与实现一致。

交付时必须说明：

- 修改了哪些核心行为；
- 是否影响历史数据和 API 兼容性；
- 执行了哪些验证；
- 哪些生产停服、迁移或凭据操作尚需运维执行。
