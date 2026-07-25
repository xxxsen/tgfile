# tgfile 核心链路修复实施与验证记录

实施日期：2026-07-25  
实施依据：`docs/02-safe-fix-implementation-plan.md`  
实施范围：C-01～C-11  

工程规范、lint 配置及后续零告警整改见
`docs/04-engineering-quality-and-lint.md`。

## 1. 实施结论

`docs/02-safe-fix-implementation-plan.md` 中 C-01～C-11 已全部落地。代码、自动化测试、
竞态检查、漏洞扫描、本地构建和容器构建均已通过。

本次在工作区中没有连接、读取或修改生产数据库，也没有删除任何生产文件、Telegram
消息、文件记录或分片记录。所有会写数据库的迁移验证均使用 `t.TempDir()` 下的临时
SQLite 数据库，并在测试内完成 forward/reverse。

生产环境仍需在停服窗口中执行审计和 `/defauls` 到 `/defaults` 的单行事务迁移。历史
异常数据继续遵守“先审计、后人工确认”的规则，本次不自动删除 Draft、无引用 file_id、
缺失 mapping 或任何后端块。

## 2. 逐项实施结果

| 编号 | 已实施内容 | 历史数据影响 | 验证结果 |
|---|---|---|---|
| C-01 | 新增只读 `audit` 维护模式；只解析 `db_file`，使用 SQLite `mode=ro` 和 `query_only`；报告文件强制为 `0600` | 无 | 审计前后数据库 SHA-256 一致；只读连接写入失败；异常统计和权限测试通过 |
| C-02 | 配置启动日志改为 `SafeLogFields` 白名单；不输出 Bot Token、用户密码、`bot_config`、`user_info` | 无 | Token/密码哨兵日志测试通过 |
| C-03 | 直链 key 严格按正式格式解析；非法输入返回 400；新增 `check-key` 维护命令 | 无；历史非正式别名会被拒绝 | 单元测试、HTTP 畸形 key 测试和 fuzz 通过，无 panic |
| C-04 | 默认路径改为仅使用 `/defaults/`；新增 forward/reverse、dry-run 和事务迁移命令；不做 `/defauls` fallback | 生产执行时只修改根目录 mapping 的 `file_name` 一列 | 单行数、字段、子节点数、冲突回滚、正反迁移和历史直链内容一致性测试通过 |
| C-05 | 删除 `cmd/tgc/`、`tgc/`、服务端 multipart handler/router/model；清理 release、README 和依赖 | 不修改存量数据；旧 tgc 和 multipart API 不再兼容 | `go list ./...` 无 tgc；源码/Action/README 无有效 tgc 引用；三个旧路由均返回 404 |
| C-06 | 上传前校验 size/block size/最大分片数；无溢出计算；末片精确限长；短读不能 Ready | 只约束新上传；失败可留下可审计 Draft，不创建 mapping | -1、0、1、5、8 字节和超分片测试通过；短读无 Ready |
| C-07 | S3 PUT 先查重；同路径进程内串行；已存在对象返回 409 且不上传；数据库唯一约束保留为多实例最终防线 | 不覆盖既有对象；并发失败的新文件只审计、不自动删 | 同名无上传、同路径并发一成一冲突、不同路径不互锁、原内容不变测试通过 |
| C-08 | Telegram 注入带固定超时的 HTTP Client；上传 Reader 响应 context；上传不重试；只读请求仅重试 429/500/502/503/504 和临时网络错误 | 无 | 超时配置、取消、只读重试、404 不重试、上传只调用一次测试通过 |
| C-09 | 使用显式 `http.Server`；配置 ReadHeader/Idle/Header 上限；SIGINT/SIGTERM 优雅关闭 30 秒；退出关闭 SQLite | 无 | 超时配置、预取消、在途请求完成和监听关闭测试通过 |
| C-10 | Ristretto Set 等待并验证可见性；明确 L2 文件所有权；reject/evict 清理缓存文件；校验缓存配置 | 只影响可重建缓存 | 立即可见、策略拒绝、当前读取成功、无永久孤儿缓存文件和 race 测试通过 |
| C-11 | 所有测试使用临时数据库/缓存和 `httptest`；新增 S3/直链集成测试；新增 CI 并加固两个 tag workflow | 无 | S3 PUT/GET/HEAD/Range、直链、迁移、删除路由和全部质量门禁通过 |

## 3. 关键代码落点

- 维护与迁移：`maintenance/audit.go`、`maintenance/migrate.go`、
  `maintenance/database.go`、`cmd/cmd.go`
- 脱敏：`config/config.go`、`cmd/cmd.go`
- 直链：`server/handler/file/file_common.go`
- 上传完整性：`filemgr/file_manager_impl.go`
- S3：`server/handler/s3/object.go`、`server/handler/s3/s3.go`
- Telegram：`blockio/telegram/tg_blockio.go`
- HTTP 生命周期：`server/server.go`、`db/db.go`
- 缓存：`filemgr/file_io_cache.go`、
  `cacheapi/adaptor/ristretto_cache_adaptor.go`
- 集成测试：`server/integration_test.go`
- CI/发布：`.github/workflows/ci.yml`、
  `.github/workflows/build_release.yml`、`.github/workflows/build_tag.yml`

## 4. 最终验证记录

验证使用 Go 1.25.12。

| 验证命令/项目 | 结果 |
|---|---|
| `test -z "$(gofmt -l .)"` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 通过 |
| `go test -race ./...` | 通过 |
| `go test ./server/handler/file -run '^$' -fuzz '^FuzzExtractLinkFromFileKey$' -fuzztime=3s` | 通过，343,495 次执行 |
| `govulncheck ./...` | 通过，0 个可达漏洞 |
| `go mod verify` | 通过 |
| `go build -trimpath -o /tmp/tgfile-final-build/tgfile ./cmd` | 通过 |
| `docker build --pull -t tgfile:codex-verification .` | 通过，镜像内维护命令验证通过 |
| workflow YAML 解析 | 通过 |
| `git diff --check` | 通过 |
| tgc 包、代码、Action、README 静态残留检查 | 通过 |
| `make check`（含 golangci-lint v2.11.4） | 通过，`0 issues` |

`govulncheck` 仍提示依赖包或依赖模块中存在当前代码不可达的漏洞，但明确报告本项目
受影响的可达漏洞为 0。Go 版本已在 `go.mod`、Dockerfile 和 CI 中统一为 1.25.12。

## 5. 生产上线前置操作

生产操作必须以 `docs/02-safe-fix-implementation-plan.md` 第 7～9 节为唯一命令手册，
一次停服完成，不做分阶段上线。以下项目不可遗漏：

### 5.1 窗口前

1. 准备并复核新、旧镜像的不可变 digest，提前拉取，禁止使用 `latest`。
2. 保存本次 CI 结果；确认格式、vet、test、race、govuln 和镜像构建全部成功。
3. 确认 `sqlite3`、`sha256sum`、`curl`、AWS CLI 可用。
4. 复核 Compose、配置、数据、SQLite、备份和持久化 L2 缓存的绝对路径。
5. 确认备份空间至少为数据库文件和 WAL 总大小的两倍。
6. 检查最近 14 天没有 multipart 调用；归档 Draft 清单，不删除。
7. 选取至少 20 个直链和 20 个 S3 存量样本；不足则全量。保存长度、完整或 Range
   SHA-256，以及现有 ETag/MD5。
8. 准备正式配置和旧镜像回滚配置；两者都必须使用轮换后的新 Token/密码。
9. 明确上线、验证、回滚负责人和最长停服时间。

### 5.2 停服后、更新镜像前

1. 网关先关闭 S3 PUT 和 `/file/upload`。
2. 停止所有 tgfile 实例；确认端口不监听、没有其他实例连接同一个数据库。
3. 执行 WAL checkpoint，再用 SQLite `.backup` 生成备份；对备份执行
   `PRAGMA quick_check` 和 SHA-256；同时备份配置和 Compose 文件。
4. 用新镜像执行只读 audit 并保存 `audit-before.json`。
5. 必须确认 `quick_check=ok`、`root_count=1`、`source_count=1`、
   `target_count=0`、`source_kind=1`、`source_is_directory=true`。
6. 持久化 L2 缓存只能先改名保留，再创建同 owner/group/mode 的空目录；不能直接删除。
7. 执行 forward dry-run；前置条件完全一致后才能执行 `dry-run=false`。
8. 正式迁移必须输出 `changed_rows=1`。
9. 迁移后再次执行 quick_check、表计数、根目录字段对比和 audit；除根目录
   `file_name` 外不得有业务行变化，异常数量不得增加。
10. 任一检查失败都不得更新镜像；优先 reverse，reverse 不满足条件时从 SQLite
    备份恢复。

### 5.3 更新和启动

1. Compose 中只替换为已复核的新镜像 digest。
2. 轮换 Telegram Bot Token 和上传账号密码；确认新 Token 仍属于原 Bot，且不把凭据
   输出到终端或日志。
3. 只启动一个实例，不拆分功能、不并行启动旧版本。
4. 确认实际运行 digest 正确，启动日志中无凭据、panic、数据库或缓存初始化错误。

### 5.4 启动后放量前

1. 验证全部直链和 S3 存量样本的状态码、长度、Range 和 hash。
2. 使用全新 S3 key 执行 PUT、HEAD、GET；再次 PUT 必须为 409 且原内容不变。
3. 畸形直链 key 必须为 400，服务无 panic。
4. `/file/multipart/begin`、`part`、`end` 必须全部为 404。
5. 再次 audit，要求 `quick_check=ok` 且历史异常数量不增加。
6. 观察 Telegram、HTTP、缓存错误率和资源使用，验证完成后才重新开放 S3 PUT。
7. 按保留策略处理可能含旧凭据的历史日志；生产冒烟对象只登记，不自动删除。

## 6. 数据清理与迁移边界

本次唯一允许自动执行的生产业务数据修改是：

```text
根节点下目录 mapping：file_name = "defauls" -> "defaults"
```

迁移命令还会校验并保持该行的 `entry_id`、`parent_entry_id`、`file_kind`、`ctime`、
`mtime`、`ref_data`、`file_size`、`file_mode` 和子节点数量不变。

以下异常只进入审计报告，不在本轮自动清理：

- mapping 指向不存在的 file_id；
- mapping 指向非 Ready 文件；
- Ready 文件声明分片数与实际分片数不符；
- 无引用 file_id、Draft 和失败上传留下的分片；
- 生产冒烟对象；
- Telegram 消息或其他后端块。

这些数据的后续人工迁移或 GC 步骤见实施方案第 6 节。该限制是为了避免把历史可读数据
误判为无效数据并造成不可恢复删除。

## 7. 已知兼容性变化

1. `/defauls` 无兼容期：新镜像只能连接已迁移为 `/defaults` 的数据库；旧镜像只能
   连接已 reverse 为 `/defauls` 的数据库。
2. tgc 二进制和 `/file/multipart/*` API 已删除，调用方必须使用 S3 PUT。
3. 非正式直链别名会返回 400；服务端历史正式 canonical key 不变。
4. S3 PUT 已存在对象继续返回 409，不提供覆盖语义。
5. WebDAV、备份格式、后端 Delete/GC、历史 MD5、访问模式和外键改造仍按实施方案
   第 5 节延期，不属于本次 C-01～C-11。
