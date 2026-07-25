# tgfile 工程规范与 lint 整改记录

实施日期：2026-07-25  
参考工程：`/home/sen/work/metaloom`  
适用版本：Go 1.25.12、golangci-lint v2.11.4

## 1. 实施结论

本次已补齐项目级 AI Agent 规范、固定版本 lint 配置和统一质量门禁，并完成存量代码的
全部 lint 整改。最终结果：

- `AGENTS.md` 已建立，明确数据兼容、文档、测试、lint、日志和交付约束；
- `.golangci.yml` 已建立，启用 correctness、security、maintainability 和架构边界检查；
- `Makefile` 已提供 `test`、`test-race`、`lint`、`check` 和固定版本安装命令；
- golangci-lint 从首次执行的 523 个问题收敛到 `0 issues`；
- `make check` 全量通过，包括格式、vet、普通测试、race 测试和 lint；
- 原临时目录中的正式文档已迁入 `docs/`，并按两位序号命名。

本次没有连接或修改生产数据库，没有读写生产文件块，也没有执行生产迁移。

## 2. 新增工程门禁

### 2.1 Agent 规范

`AGENTS.md` 固定了以下关键约束：

1. 存量 S3 GET/HEAD、Range 和文件直链读取优先保持兼容。
2. 不得自动删除 Telegram 消息、文件块、Draft、分片或无引用文件。
3. 默认上传根目录固定为 `/defaults`，运行时代码不得增加历史路径 fallback。
4. 短期工作目录不持久化正式文档，正式文档必须进入 `docs/`。
5. 代码交付前必须通过 `make check`。

### 2.2 golangci-lint

`.golangci.yml` 使用 v2 配置格式，主要启用：

- 正确性：`govet`、`staticcheck`、`nilerr`、`errcheck`、`errorlint`、
  `rowserrcheck`、`sqlclosecheck`；
- 安全性：`gosec`、`bodyclose`、`noctx`、`contextcheck`；
- 可维护性：`revive`、`gocognit`、`gocyclo`、`nestif`、`lll`、
  `nonamedreturns`；
- 工程边界：`depguard`、`forbidigo`、`wrapcheck`、`gochecknoinits`。

生产迁移完成后，历史路径字符串已从 Go 源码移除，`misspell` 的临时忽略规则也同步
删除。

### 2.3 Makefile

质量命令固定为：

```shell
make install-golangci-lint
make lint
make check
```

`make check` 顺序执行：

1. `gofmt`；
2. `go vet ./...`；
3. `go test ./...`；
4. `go test -race ./...`；
5. golangci-lint。

当 `GO` 指向绝对路径时，Makefile 会把对应目录加入 lint 子进程的 `PATH`，便于 CI
或隔离工具链稳定复现。

## 3. lint 整改内容

### 3.1 不改变数据语义的整改

- 为数据库、文件、网络和接口错误补充 `%w` 错误链；
- 检查类型断言、关闭数据库 Rows、HTTP body、文件和压缩流；
- HTTP、数据库和测试调用补充 context；
- 对整数转换增加边界检查，避免分片数、分页值和日志配置溢出；
- 拆分过大的接口和高复杂度函数；
- 统一 receiver、未使用参数、行宽和格式；
- 缓存临时文件只在目录遍历完成后删除，避免遍历期间修改目录；
- 对扫描 batch 增加非零和上限校验，并保留不足一批时正确退出的语义。

### 3.2 lint 发现并修复的真实缺陷

1. `dao/file_part_dao.go` 首次插入成功时原来返回 `nil, nil`，现返回非空成功响应。
   该修复只影响新上传调用结果，不修改历史记录。
2. rotate 读取对负旋转值使用 Go 负余数，低字节可能产生错误结果；现先归一化到
   `[0, 255]`。现有 `rotate_stream` 配置和存量块编码保持不变。
3. 文件流在异常 BlockIO 返回非正 block size 时可能除零；现返回可分类错误。
4. 备份导出原来在遍历闭包中延迟关闭文件，可能长时间占用大量句柄；现每个文件写完
   立即关闭，并检查 tar/gzip 收尾错误。
5. L2 缓存文件名解析不再依赖“解析失败后返回 nil error”的写法，避免把真实控制流
   混同为错误吞掉。

### 3.3 明确保留的兼容行为

- 不修改 SQLite schema、表数据和字段编码；
- 不修改 file_id、FileKey、分片顺序、S3 路径或外部直链 key；
- 存量 MD5 继续使用原格式，只作为兼容校验值，不作为安全哈希；
- 一次性路径迁移和反向回滚代码在生产迁移验证后退休；
- Telegram、localfile 和 mem 的 BlockIO 注册方式保持不变；
- 本轮 lint 整改不新增任何生产前置数据迁移。

## 4. 文档整理

正式文档现为：

1. `docs/01-code-review.md`
2. `docs/02-safe-fix-implementation-plan.md`
3. `docs/03-implementation-verification.md`
4. `docs/04-engineering-quality-and-lint.md`
5. `docs/05-one-time-migration-retirement.md`

源码、README 和正式文档不再引用临时工作路径。

## 5. 最终验证

执行命令：

```shell
make check GO=/tmp/codex-go1.25.12.GvW9HE/go/bin/go
```

结果：

| 检查项 | 结果 |
|---|---|
| `gofmt` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 通过 |
| `go test -race ./...` | 通过 |
| golangci-lint v2.11.4 | `0 issues` |
| `git diff --check` | 通过 |
| 临时目录正式文档残留 | 0 |

## 6. 生产影响

本文件所述 lint 整改本身不要求生产数据迁移。首次生产迁移已经完成，
`docs/02-safe-fix-implementation-plan.md` 现仅作为历史上线记录；后续版本不得重复
执行其中的一次性路径迁移。
