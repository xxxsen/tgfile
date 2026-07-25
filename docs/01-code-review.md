# tgfile 代码审查与改进建议

审查日期：2026-07-25  
审查范围：仓库内 92 个 Go 文件（约 6,880 行，含测试）、构建脚本、Dockerfile、GitHub Actions、README。  
审查方式：核心链路走读、边界条件分析、`go vet ./...`、`go test ./...`（在临时 Windows/Go 1.25 环境中执行）。
实施状态：核心链路修复已完成，见 `docs/03-implementation-verification.md`。
一次性路径迁移能力已在生产迁移完成后退休，见
`docs/05-one-time-migration-retirement.md`。

## 1. 结论摘要

项目的整体分层是清楚的：HTTP/S3/WebDAV 协议层通过 `filemgr` 统一访问文件，元数据落 SQLite，文件块由 `blockio` 接入 Telegram、本地文件或内存；L1/L2 缓存也与底层存储解耦。目录操作已有相对丰富的测试，S3 HEAD、数据库审计等近期功能也有针对性用例。

当前主要风险集中在“上传完整性、凭据保护和长期运行可靠性”：

1. 普通上传依赖请求声明的文件大小，原实现没有验证 Reader 是否提前结束，也没有精确
   限制末片长度。
2. 服务启动时会把包含 Bot Token 和全部用户密码的配置对象写入日志。
3. S3/WebDAV 覆盖上传不是原子替换：先上传数据，再创建链接；链接已存在时返回错误，
   刚上传的数据成为孤儿。
4. 底层存储没有删除接口，失败上传和无引用文件缺少完整生命周期管理。
5. 测试没有形成可靠的 CI 闭环，缓存测试还暴露了同步语义与 Ristretto 异步写入
   不一致的问题。

建议先完成 P0/P1 项，再扩展协议能力或做性能优化。

## 2. 架构梳理

主要调用链如下：

```text
HTTP / S3 / WebDAV / Backup
              │
              ▼
              filemgr.IFileManager
              ├── 文件状态与分片：fileDao / filePartDao
              ├── 路径与目录：fileMappingDao / directory
              ├── L1/L2 I/O 缓存：fileIOCache
              └── 文件块：blockio
                  ├── Telegram
                  ├── localfile
                  └── mem
                    │
                    ▼
                  SQLite
```

值得保留的设计：

- `blockio.IBlockIO`、`filemgr.IFileManager` 和 DAO 的接口边界清晰，便于测试和替换实现。
- 多数 I/O 链路传递了 `context.Context`，下载端也支持 Range/Seek。
- 目录的 Copy/Move/Remove 放在数据库事务中，SQL 主要通过 builder 参数化生成。
- L2 缓存通过临时文件加 Rename 落盘，避免直接暴露未完成文件。
- S3 HEAD 不打开文件流，错误响应也已区分 HEAD 与 GET。

## 3. 问题清单

优先级定义：

- P0：可能导致密钥泄露、跨用户数据破坏或核心数据不可信，应立即修复。
- P1：明显影响可靠性、安全边界或长期运行，建议近期完成。
- P2：协议兼容性、性能、可维护性和工程质量改进。

### F-01 [P2] 服务启动和维护操作混用同一个 flag 入口

证据：

- 原 `cmd` 入口同时注册服务配置、审计输出、迁移方向、dry-run 和 key 校验参数。
- `-maintenance` 再通过字符串选择三种完全不同的离线操作。
- 不同操作的参数可以同时出现，命令帮助无法准确表达哪些参数属于哪项操作。

影响：

- 运维容易把迁移、审计和服务启动参数混用。
- 无法为各操作单独定义必填参数、帮助信息和退出码。
- 后续新增维护功能会继续扩大根入口的条件分支。

建议：

上线版本先使用 Cobra 拆分服务、审计、一次性迁移和 key 校验；生产迁移完成后退休
一次性迁移命令。当前保留 `serve`、`audit` 和 `check-key`，每个子命令只注册自身
参数，根命令不承载业务 flag。

验收用例：

- 根命令未指定子命令时返回明确错误。
- `audit` 不接受 `check-key` 的参数。
- 每个子命令的帮助只展示自身参数。
- Docker 默认启动和开发脚本显式使用 `serve`。

### F-02 [P0] 上传缺少尺寸和实际字节数校验

证据：

- `filemgr/file_manager_impl.go`：原实现直接按声明大小计算分片，未先拒绝负数、
  非正 block size 和过大的分片数量。
- 普通上传原来每片使用完整 `MaxFileSize()`，不检查 Reader 是否提前 EOF，也不限制
  最后一片为“剩余大小”。
- `server/handler/backup/import.go:73-80`：归档头中的 `hdr.Size` 同样被直接信任。

影响：

- 超大声明、异常 block size 或短读都可能形成错误状态或资源消耗。
- 声明大小与实际数据不一致时，下载、Range 和缓存行为不可预测；空块配合非零声明大小还可能反复回源。
- 超大声明值可能产生极多上传循环或数据库垃圾。

建议：

1. 校验非负文件大小、正 block size 和最大分片数。
2. 普通上传每片限制为 `min(block_size, remaining)`。
3. 使用计数 Reader 确保实际读取值等于声明值。
4. 短读时不得把文件标记为 Ready，也不得创建路径 mapping。
5. `MarkFileReady` 检查 RowsAffected，并带原状态条件。

### F-03 [P0] 启动日志泄露 Bot Token 和用户密码

证据：

- 原单体命令入口使用 `logger.Info("recv config", zap.Any("config", c))` 输出整个配置。
- `config/config.go:36-47`：配置包含 `BotInfo` 和 `UserInfo`；Telegram Token 位于 `BotInfo`，密码位于 `UserInfo`。

影响：

- 控制台、日志文件、容器日志平台和故障采集系统都会保存明文密钥。
- 一旦日志可读，攻击者可接管 Telegram Bot，并取得所有上传接口凭据。

建议：

- 删除完整配置日志，改为显式记录非敏感字段，如 bind、bot_kind、功能开关、bucket 数量和缓存大小。
- 为配置类型实现专用的 `SafeLogFields()`，不要依赖通用结构序列化。
- 轮换曾经出现在日志中的 Token 和密码，并清理历史日志。
- 增加单元测试，保证日志字段中不出现 token、secret、password 和 `user_info` 值。

### F-04 [P1] 文件 key 解析可被公开请求触发 panic

证据：

- `server/handler/file/file_common.go:52-68`：代码找到 `-` 后检查的是整个 `fkey` 长度，而不是 `prefix` 长度，随后执行 `prefix[:2]`。
- 例如 `a-x` 或 `-xx` 的总长度大于 2，但前缀长度不足 2，会触发 slice bounds panic。
- `/file/download/:key` 和 `/file/meta/:key` 在 `server/server.go:44-45` 中不要求认证。

当前全局 Recover 会把进程级崩溃降为 500，但每次都会生成错误和堆栈日志，仍可被用于低成本日志放大与服务扰动。

建议：

- 不要先“清理”再接受 key，应对原始输入做严格格式校验。
- 当前 hash 是 8 字节十六进制，建议格式固定为 `^[0-9a-f]{16}-<safe-name>$`，并单独限制总长度。
- 所有切片前检查对应局部变量长度。
- 增加畸形 key 的 fuzz test，断言永不 panic。

### F-05 [P1] 覆盖上传不符合 S3/WebDAV 语义，并产生孤儿块

证据：

- `server/handler/s3/object.go:67-77`、`server/handler/webdav/put.go:12-24`：先完整创建文件，再创建路径映射。
- `directory/db_directory.go:557-586`：目标已存在时返回 `os.ErrExist`，没有覆盖。
- `server/handler/file/file_upload.go:24-32` 和分块 Finish 也存在“文件完成后创建链接失败”的窗口。

影响：

- S3 PutObject 和 WebDAV PUT 对已有对象通常应替换，而当前实现返回 500。
- 链接冲突、数据库错误或客户端重试后，新文件块已经上传但没有引用。
- 本地块占用磁盘；Telegram 消息也会永久残留。

建议：

- 增加原子 `CreateOrReplaceFileLink`，在事务中完成目标路径交换，并返回旧 file_id。
- 新建返回 201，替换返回 200/204；冲突映射为 409/412，不要统一返回 500。
- 请求失败时清理本次上传的元数据和可删除的后端块。
- 为请求增加 idempotency key，避免客户端重试产生多个文件。

### F-06 [P1] 底层块没有生命周期删除能力，Purge 只删索引

证据：

- `blockio/blockio.go:12-17`：接口只有 Upload/Download，没有 Delete。
- `filemgr/file_manager_impl.go:248-258`：Purge 只删除分片表和文件表记录。
- 上传中途失败、链接创建失败、分片覆盖都会留下旧块。
- `filemgr/file_manager_impl.go:261-293`：Purge 将候选 file_id 全量保存在内存中，再扫描全部映射，规模增长后内存和耗时均为 O(N)。

建议：

- 为支持删除的后端增加 `Delete(ctx, filekey)`，能力可通过可选接口表达。
- 采用“数据库先标记待删除 -> 异步删除块 -> 成功后删除元数据”的可重试状态机，避免部分失败。
- Telegram 若无法真正删除，也应记录 orphan 指标，并可选保存 message ID 以便调用删除消息接口。
- 使用 SQL `NOT EXISTS`/反连接和分页处理无引用文件，不要把全集合放入内存。
- 明确上传 session TTL；活跃分片应刷新 session 时间，避免被一天阈值误清理。

### F-07 [P1] 重传分片时 MD5 没有更新

证据：

- `dao/file_part_dao.go:60-63`：重复键更新只写 `file_key` 和 `mtime`，未写 `file_part_md5`。
- `filemgr/file_manager_impl.go:151-169`：Finish 使用数据库里的分片 MD5 计算最终值。

影响：

- 分片重传成功后，文件内容已经变化，但元数据仍保留旧 MD5。
- 多片文件的整体 MD5 继续基于错误值计算。

建议：

- Upsert 时同时更新 `file_part_md5` 和 `part_size`。
- 将“上传后端块 + 更新分片引用”设计成可补偿流程；替换成功后清理旧块。
- 增加“同一 part_id 重传不同内容”的回归测试。

### F-08 [P1] Telegram 上传忽略取消信号且没有请求超时

证据：

- `blockio/telegram/tg_blockio.go:61-74`：`Upload(ctx, ...)` 完全没有使用 `ctx`。
- 当前依赖的 `tgbotapi.NewBotAPI` 内部使用无 Timeout 的 `http.Client`，`Send` 也不接收 context。
- 下载虽然使用 `NewRequestWithContext`，但 Transport 未设置 `ResponseHeaderTimeout`。

影响：

- 客户端断开、服务关闭或上层超时后，上传仍可能长期占用 goroutine 和网络连接。
- Telegram 或网络异常时，初始化、上传和下载可能挂住较长时间。

建议：

- 用 `NewBotAPIWithClient` 注入配置了连接、TLS、响应头和整体超时的 HTTP Client。
- 若库无法传递 context，封装/替换其上传实现，确保请求使用 `NewRequestWithContext`。
- 统一定义可配置的回源超时、重试次数和指数退避，并只重试可重试错误。
- 暴露 Telegram 请求耗时、重试、失败和在途上传指标。

### F-09 [P1] HTTP 服务缺少显式超时、优雅关闭和请求级资源上限

证据：

- `server/server.go:91-93` 直接调用依赖的 Engine.Run。
- 当前 `github.com/xxxsen/common/webapi@v0.1.30` 的实现最终调用 Gin `Run`，没有自定义 `http.Server` 超时，也没有 Shutdown 入口。
- 已知 Content-Length 的上传没有项目级最大请求体限制；依赖中只有未知长度 chunked 请求的 5 MiB 限制。

影响：

- 慢请求、超大请求和异常连接可能长期占用连接、临时文件、内存与后端上传配额。
- 进程停止时没有等待在途请求或关闭数据库/缓存。

建议：

- 使用显式 `http.Server`，至少配置 `ReadHeaderTimeout`、`IdleTimeout`、`MaxHeaderBytes`，并结合上传场景审慎配置 Read/Write 超时。
- 监听 SIGINT/SIGTERM，调用 `Shutdown(ctx)`；关闭 SQLite、缓存和底层连接。
- 对普通 multipart、单分片、备份导入分别使用 `http.MaxBytesReader` 或计数 Reader。
- 增加全局/用户级并发上传限制与速率限制。

### F-10 [P1] 备份导入不是原子操作，导出会累积打开的文件句柄

证据：

- `server/handler/backup/export.go:44-60`：遍历循环内 `defer stream.Close()`，直到整个导出结束才统一执行。
- `server/handler/backup/import.go:35-69`：文件边读边正式导入，最后才检查 `statistic.txt`；归档无 manifest 或中途损坏时已产生部分副作用。
- `server/handler/backup/import.go:73-80`：信任 tar Header.Size，没有总文件数、总展开大小或单文件大小限制。
- Export/Import 都无法在流式响应后半段出错时给客户端可靠的结构化结果。

影响：

- L2 命中时每个文件对应一个 `os.File`，大量文件导出可能耗尽文件描述符。
- 无效归档可以写入若干文件后返回 400；重试又会因路径已存在而失败。
- 压缩炸弹或恶意 Header 可放大后端上传与数据库写入。

建议：

- 每轮复制完成立即 Close，并处理 Close 错误，不要在循环中 defer。
- 备份格式增加版本化 manifest，包含文件数、总大小、每文件校验和；先读取和验证 manifest。
- 导入到 staging namespace/session，全部校验成功后事务性切换；失败时清理 staging。
- 设置归档字节数、展开总大小、文件数、路径长度、单文件大小上限。

### F-11 [P1] 缓存接口语义与 Ristretto 异步写入不一致

证据：

- `cacheapi/adaptor/ristretto_cache_adaptor.go:26-28`：忽略 `Set` 返回值，直接报告成功。
- `cacheapi/cacheapi.go:70-73`：调用方也忽略缓存 Set 结果。
- `filemgr/file_io_cache.go:88-96`：L2 文件落盘后异步放入缓存；被拒绝时磁盘文件没有确定的回收路径。
- 本次执行 `filemgr/TestFileIOCache` 时，即使预先创建测试目录，紧接 Set 的 Get 仍多次返回 `cache key not exist`，与测试和上层直觉中的同步语义不一致。

影响：

- 同一热点文件并发 miss 时会重复回源、重复写盘。
- Set 被拒绝或尚未生效时，磁盘缓存文件可能不受容量淘汰管理。
- 同一个 file_id 的并发下载、Rename 和 Evict 可能互相删除正在登记的缓存路径。

建议：

- 明确缓存是最终一致还是读己之写；若需要同步语义，在适当位置调用 Ristretto `Wait()` 或更换实现。
- 不要吞掉 Set/Del 结果；至少记录拒绝指标并清理未接管的 L2 文件。
- 使用 `singleflight` 合并同一 file_id 的并发回源。
- 启动加载时校验文件实际大小与文件名编码大小一致，坏缓存应删除并回源。
- 将 L2 元数据和文件生命周期封装在同一组件中，补充并发与重启恢复测试。

### F-12 [P1] 测试不够可重复，CI 没有在常规变更上运行测试

本次验证结果：

- `go vet ./...`：通过。
- `go test ./...`：失败。
  - `filemgr/TestFileIOCache` 硬编码 `/tmp/tgfile-cache`；目录不存在时测试 panic。创建目录后仍因 Ristretto 异步 Set 与立即 Get 的假设不一致而失败。
  - `server/handler/s3/s3base/TestS3UploadDownload` 假设 `127.0.0.1:9901` 已运行服务；连接失败后只使用 `assert`，继续对 nil Reader 调用 `io.ReadAll` 并 panic。
  - 同次执行中的其余包通过。
- `go test -race`：当前可用的 Windows Go 环境未启用 CGO，未进入测试执行阶段。

其他证据：

- `filemgr/file_io_cache_test.go:14-75` 使用固定目录，没有 `t.TempDir()` 和错误后的 `require`。
- `server/handler/s3/s3base/s3_base_test.go:13-28` 实际是外部集成测试，却混在默认单元测试中。
- 多个包通过全局 `TestMain` 和固定 `/tmp/*.db` 共享数据库，限制并行且容易残留。
- `.github/workflows` 只有 tag/release 构建；没有 PR/push 的 fmt、vet、test、race、govulncheck。
- `go.mod` 声明 Go 1.25，而 release workflow 仍配置 Go 1.21，工具链意图不一致。

建议：

1. 所有文件/数据库测试改用 `t.TempDir()`，每个测试独立创建依赖。
2. 测试先 `require.NoError` 再使用返回值，避免一个断言失败引发二次 panic。
3. S3 端到端测试用 `httptest.Server` 自包含；确需外部服务的测试加 build tag 或显式 integration job。
4. 新增 PR CI：`gofmt -l`、`go vet ./...`、`go test ./...`、`go test -race ./...`、`govulncheck ./...`。
5. 对 key parser、短读、超长输入、覆盖上传、备份损坏、并发缓存增加表驱动和
   fuzz 测试。

### F-13 [P2] 错误链和资源关闭处理不完整

证据：

- 多个 DAO、文件流和协议 handler 直接返回外部错误，缺少操作上下文。
- 备份导出在遍历回调中延迟关闭文件，文件数量大时会持续占用句柄。
- gzip、tar、HTTP body、数据库 Rows 和临时文件清理存在未检查的关闭错误。
- 部分整数转换没有先验证范围。

建议：

- 使用 `%w` 保留错误原因链，并为跨层操作补充明确上下文。
- 文件和归档流在最小作用域内关闭，必要时使用 `errors.Join` 合并业务和关闭错误。
- 数据库查询统一检查 `Rows.Err()` 并关闭 Rows。
- 所有窄化整数转换先做上下界校验。

### F-14 [P2] 配置缺少集中校验和安全默认值

证据：

- `config/config.go:49-64` 只做 JSON Unmarshal；未知字段、空 bind、空数据库路径、无用户、非法缓存大小、localfile block_size <= 0 等都不会在解析阶段失败。
- `filemgr/file_manager_impl.go:116-122` 假设底层 MaxFileSize 大于 0。
- `filemgr/file_io_cache.go:218-275` 用配置值计算 Ristretto 容量，零或负数缺少友好校验。

建议：

- 增加 `Config.Validate()`，启动前一次性返回字段路径明确的错误。
- 使用 `json.Decoder.DisallowUnknownFields()`，降低拼写错误导致静默使用零值的风险。
- 验证 bind、数据库父目录、用户列表、bucket 名、WebDAV root、block size、缓存容量/单项上限及它们之间的关系。
- 日志输出只展示校验后的非敏感摘要。

### F-15 [P2] 协议和可见性策略需要显式化

证据：

- `server/server.go:44-45,72-73`：原生下载、meta、S3 GET/HEAD 默认公开。
- `server/httpkit/httpkit.go:18-23`：所有下载默认 `Cache-Control: public, max-age=604800`。
- README 也将 GET 标记为无需鉴权。

这可能是分享链接产品的有意设计，不一定是代码缺陷；但 S3/WebDAV 常被用户理解为私有存储，当前策略容易误用。

建议：

- 增加明确的 `public_read` 配置，默认值和升级行为写入文档。
- 私有模式要求认证或使用有时效的签名下载 URL。
- 私有响应使用 `Cache-Control: private/no-store`；公开对象再使用 public cache。
- 文档强调 Basic Auth 必须置于 HTTPS 反向代理之后。

### F-16 [P2] 文件元数据与部分标准语义仍需收敛

问题：

- `filemgr/file_manager_impl.go:159-169` 对多片文件计算的是“各分片 MD5 十六进制字符串拼接后再 MD5”，不是完整文件 MD5；字段和 API 却统一叫 `md5`。
- 空文件 MD5 为空字符串，而不是标准空内容 MD5。
- S3 Put 没有返回 ETag，README 也只声明部分 S3 能力。
- `filemgr/file_system_entry.go:91-103` 的 `ReadDir(n)` 每次返回前 n 项，不维护游标，也不按 `fs.ReadDirFile` 约定在读完后返回 `io.EOF`。
- `server/handler/file/file_common.go:28-39` 按字节截断 UTF-8 文件名，可能产生非法 UTF-8。
- `utils/fileid_utils.go:16-21` 的未使用 Decode 函数在解码结果少于 8 字节时会 panic。

建议：

- 明确字段为 `content_md5` 或 `multipart_etag`；若承诺完整 MD5，应在上传过程中按内容顺序计算。
- 完善 S3/WebDAV 支持矩阵和对应状态码测试。
- 修正 `ReadDir` 游标语义，文件名按 rune/规范化后截断。
- 删除未使用工具函数，或对解码长度做严格检查。

### F-17 [P2] 构建和运行镜像需要现代化

证据：

- `Dockerfile:7` 使用已经停止维护的 `alpine:3.12`。
- 容器默认以 root 运行，没有健康检查。
- GitHub Actions 仍使用 `checkout@v2/v3`、`setup-go@v2` 和早期 Docker actions。
- release workflow 的 Go 1.21 与 `go.mod`/Dockerfile 的 Go 1.25 不一致。

建议：

- 升级到仍受支持并固定 digest 的基础镜像，加入 CA 证书和时区等实际运行依赖。
- 创建非 root 用户，数据、配置和缓存目录分别声明权限；配置建议只读挂载。
- 增加 `/healthz`（进程）和 `/readyz`（SQLite/存储依赖）并配置容器 HEALTHCHECK。
- 更新 Actions 版本，使用 `setup-go` 读取 `go.mod` 或统一固定 Go 1.25。
- 构建时注入版本、commit、build time，生成 SBOM 并做镜像/依赖扫描。

## 4. 建议的落地顺序

### 第一阶段：封住数据与密钥风险

1. 删除敏感配置日志并轮换已暴露密钥。
2. 修复公开 key parser panic。
3. 校验 file size、block size、part count 和实际读入字节数。
4. 修复分片记录更新时 MD5 未同步的问题。

完成标准：畸形请求不能 panic，短读和异常大小不能生成声明与内容不一致的 Ready 文件。

### 第二阶段：保证文件生命周期和协议可靠性

1. 实现原子路径替换和幂等上传。
2. 增加失败补偿、后端 Delete/孤儿回收状态机。
3. 给 HTTP 和 Telegram I/O 增加超时、取消、并发限制与优雅关闭。
4. 重做备份 manifest、staging 导入和资源限制。
5. 明确公开读取、缓存和 TLS 策略。

完成标准：失败和重试不会泄漏不可见文件；服务关闭可控；S3/WebDAV 覆盖与状态码符合声明。

### 第三阶段：建立持续质量与可观测性

1. 修复非自包含测试，补齐 handler/上传/缓存/备份用例。
2. 在 PR CI 中启用 fmt、vet、test、race、fuzz smoke、govulncheck。
3. 统一 Go/Actions/容器版本，升级运行镜像并以非 root 运行。
4. 增加上传、回源、缓存、SQLite、Purge/孤儿数量指标和结构化审计日志。
5. 对 Purge、目录大规模扫描和并发热点下载做基准测试。

## 5. 建议新增的关键测试

| 测试场景 | 预期 |
|---|---|
| `a-x`、`-xx`、超长及随机 file key | 4xx，永不 panic |
| 负 file_size、无效 block size、超大 part count | 明确拒绝 |
| 末片过短 | 文件保持非 Ready |
| Reader 比声明值短或长 | 明确拒绝或按已声明策略处理，不产生坏文件 |
| 同一 part_id 重传不同内容 | 内容和 MD5 同步更新，旧块进入回收 |
| S3/WebDAV 覆盖已存在文件 | 原子成功，返回正确状态，旧文件可回收 |
| 链接创建失败、客户端取消、Telegram 超时 | 无悬挂请求，临时数据可补偿 |
| 损坏/无 manifest/超限备份 | 无正式路径副作用 |
| 同一小文件百并发冷下载 | 单次或受控次数回源，无缓存文件泄漏 |
| 进程收到 SIGTERM 且存在上传 | 在截止时间内优雅退出并保留一致状态 |

## 6. 总体评价

项目已经具备可继续演进的分层基础，目录数据库和多协议入口也说明核心模型有复用价值。
当前短板不是“代码组织需要重写”，而是上传完整性、安全边界、失败补偿和工程验证
还没有跟上协议数量。优先围绕文件生命周期收紧不变量，可以在不推翻现有架构的前提下
显著提高安全性与可靠性。
