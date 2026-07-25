# AGENTS.md — tgfile AI Agent 工程规范

本文件定义 AI Agent 在 tgfile 项目中进行代码修改、测试和审查时必须遵循的规范。

## 0. 总则

- 不允许主动降低代码质量要求，包括放宽 lint、跳过失败测试、删除测试、缩小检查范围，
  或使用 `//nolint` 等方式掩盖可以修复的问题。
- 修改必须优先保证存量 S3 GET/HEAD、文件直链读取和 SQLite 数据兼容性。
- 不得自动删除 Telegram 消息、后端块、文件记录、分片记录、Draft 或无引用文件；
  数据清理必须有审计结果、明确范围、备份和可验证的迁移/回滚方案。
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

- `td/` 只允许存放临时设计、review 草稿和实施中的中间文档，不作为长期文档维护。
- 功能稳定后必须将文档整理到 `docs/`，文件名固定为 `{两位序号}-{描述}.md`，例如
  `docs/01-code-review.md`。
- `docs/`、源码、测试、注释、README 和用户可见文案中禁止引用 `td/` 或临时任务编号。
- 正式文档之间使用 `docs/` 下的稳定路径互相引用。
- 修改行为、兼容性边界或上线步骤时，必须同步更新对应正式文档。

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
- 数据迁移测试必须验证 dry-run、前置条件、提交后状态和 reverse/恢复路径。

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
- `audit`、`migrate-default-prefix` 和 `check-key` 子命令不得初始化 Telegram、
  HTTP 服务或缓存。
- 只读维护命令必须用 SQLite 只读模式，不能调用会建表或迁移 schema 的初始化逻辑。
- S3 已存在对象保持 409 语义，不得在没有新设计和迁移方案时改为覆盖。
- `/defauls` 仅允许存在于审计和 forward/reverse 迁移代码；正常读写路径只使用
  `/defaults`。
- 外部直链 key、file_id、Telegram FileKey、分片顺序和存量 MD5 不得静默改写。

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
