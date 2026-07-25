# tgfile 核心链路安全修复与迁移方案

制定日期：2026-07-25  
依据：`docs/01-code-review.md`  
线上主要链路：S3 PUT 上传、S3 GET/HEAD、`/file/download/:key` 文件直链下载。  
实施状态：C-01～C-11 已完成，逐项结果见 `docs/03-implementation-verification.md`。  

## 1. 结论

本轮只改造线上实际使用的核心链路。

本方案不会批量修改或重编码存量文件块。唯一会主动修改历史业务数据的操作是将内部目录 `/defauls` 重命名为 `/defaults`；该操作在停服窗口内完成，不实现新旧路径双读，并提供严格预检、事务迁移和反向回滚。

最终范围：

1. 保证存量 S3 对象和文件直链继续读取。
2. 加固 S3 PUT 共用的 `CreateFile` 数据完整性。
3. 修复直链 key 的公开 panic。
4. 停服迁移 `/defauls/` 到 `/defaults/`，外部直链 key 保持不变。
5. 将服务启动、审计、路径迁移和 key 校验拆分为独立子命令。
6. 修复敏感日志、Telegram 超时、HTTP 生命周期和读取缓存。
7. 建立可重复的 S3/直链测试和 PR CI。

WebDAV、备份导入导出、静态目录展示不属于本轮核心改造；保留现状，不在同一次上线中做结构性调整。

## 2. 固定兼容性边界

以下是实现时必须遵守的约束：

1. 不修改存量 `tg_file_tab` 和 `tg_file_part_tab` 的业务字段。
2. 不修改 file_id、FileKey、`ref_data`、分片顺序和现有 MD5 值。
3. 不修改 `rotate_stream` 算法或配置含义。
4. 不删除 Telegram 消息、本地块、文件记录、分片记录或无引用文件。
5. 不给历史表增加外键或强制历史数据通过新的完整性校验。
6. 存量 Ready 文件继续按原逻辑读取，不要求回填任何新字段。
7. 不改变 S3 bucket/object 路径。
8. 不改变外部 `/file/download/:key` 中的 key。
9. 不改变当前公开 GET/HEAD 和 public cache 行为。
10. S3 PUT 写已存在对象暂时继续返回冲突，不在本轮自动覆盖存量对象。
11. `/defauls/` 不做过渡期兼容；必须保证“旧镜像 + 旧路径”或“新镜像 + 新路径”成对使用。
12. 所有修复在同一个停服窗口一次性上线，不拆分生产发布批次。

## 3. 修复项与历史数据影响

| 编号 | 修复项 | 历史数据影响 | 迁移要求 |
|---|---|---|---|
| C-01 | 只读数据审计 | 无 | 无 |
| C-02 | 配置日志脱敏 | 无 | 无 |
| C-03 | 直链 key 严格校验、防 panic | 不改数据；非正式别名可能失效 | 上线前验证真实 key 样本 |
| C-04 | `/defauls/` -> `/defaults/` | 修改目录根映射名称 | 必须按第 7 节迁移 |
| C-05 | Cobra 子命令拆分 | 无 | 更新启动和维护命令 |
| C-06 | 加固 `CreateFile` 实际字节数和分块边界 | 只影响新上传 | 无历史回填 |
| C-07 | S3 PUT 错误语义和孤儿审计 | 不改历史数据 | 不自动清理 |
| C-08 | Telegram 请求超时与取消 | 不改数据 | 无 |
| C-09 | HTTP 优雅关闭 | 不改数据 | 无 |
| C-10 | L1/L2 缓存一致性 | 只影响可重建缓存 | 可清空缓存回滚 |
| C-11 | S3/直链测试隔离和 CI | 无 | 无 |

## 4. 具体实施方案

### C-01 增加只读审计模式

#### 命令

服务端二进制提供：

```shell
tgfile audit \
  --config=/config/config.json \
  --output=/tmp/tgfile-audit.json
```

#### 实现约束

审计模式：

1. 只解析配置中的 `db_file`。
2. 以 SQLite `mode=ro` 打开数据库。
3. 执行 `PRAGMA query_only=ON`。
4. 不调用当前会创建/迁移表的 `db.InitDB`。
5. 不初始化 Telegram Bot。
6. 不启动 HTTP 服务。
7. 不执行 INSERT、UPDATE、DELETE、ALTER。

#### 固定输出

```json
{
  "quick_check": "ok",
  "file_count_by_state": {},
  "file_size_by_state": {},
  "file_part_count": 0,
  "mapping_count": 0,
  "mapping_to_missing_file": [],
  "mapping_to_non_ready_file": [],
  "ready_file_part_count_mismatch": [],
  "unreferenced_file_count": 0,
  "legacy_default_root_exists": false,
  "correct_default_root_exists": false
}
```

检查项：

1. `PRAGMA quick_check`。
2. 按 `file_state` 统计文件数和总大小。
3. 文件分片总数、路径映射总数。
4. 文件映射引用不存在 file_id 的记录。
5. 文件映射引用非 Ready 文件的记录。
6. Ready 文件声明分片数与实际分片数不符的记录。
7. 没有路径引用的 file_id 数量，只统计、不删除。
8. `/defauls`、`/defaults` 是否存在。

输出不得包含：

- Bot Token；
- 用户密码；
- 完整 Telegram FileKey；
- 完整下载 key。

#### 验证标准

- 审计前后业务表逐行快照一致。
- 使用只读数据库账号/DSN 时可正常完成。
- 测试中尝试写入必须返回 readonly 错误。

### C-02 配置日志脱敏

#### 代码落点

- `cmd/serve.go`
- `config/config.go`

#### 修改

删除：

```go
logger.Info("recv config", zap.Any("config", c))
```

新增 `Config.SafeLogFields()`，只输出：

- bind；
- db_file；
- bot_kind；
- S3 enable 和 bucket 列表；
- WebDAV enable 和 root；
- L1/L2 enable 和容量；
- user_count。

禁止输出：

- `BotInfo`/`bot_config`；
- Token；
- `UserInfo` 内容；
- password、secret、access key。

#### 验证

配置中放入：

```text
TOKEN_SENTINEL_DO_NOT_LOG
PASSWORD_SENTINEL_DO_NOT_LOG
```

捕获启动日志，断言两个哨兵、`bot_config`、`user_info` 均不存在。

历史日志可能已经包含密钥，这不是代码迁移可以消除的。上线步骤必须额外执行：

1. 轮换 Telegram Bot Token。
2. 轮换上传账号密码。
3. 按日志系统保留策略清理历史敏感日志。

### C-03 修复直链 key 解析 panic

#### 保持兼容的正式格式

服务端生成的 key 为：

```text
16 个小写十六进制字符 + "-" + 最多 128 字节文件名
```

文件名允许为空和 UTF-8；本轮不改变 `buildFileKeyLink` 的 key 结构。

#### 实现

重写 `server/handler/file/file_common.go` 的输入解析：

1. 不再对用户输入执行 `removeInvalidChar` 后继续使用。
2. 要求 `17 <= len(fkey) <= 145`。
3. 要求 `fkey[16] == '-'`。
4. 要求前 16 个字节只能是 `0-9`、`a-f`。
5. 全部通过后才能访问 `fkey[:2]`。
6. suffix 可以为空和包含 UTF-8，但不能包含 `/`、`\`、NUL 或 ASCII 控制字符。
7. 校验后的 key 必须作为单个路径段使用，不能再次执行字符删除或路径归一化。
8. 校验失败统一返回 400。

禁止用正则限制 suffix 的字符集，因为历史正式文件名可能含有 UTF-8。

#### 可能的不兼容

旧实现会删除输入 key 中的非法字符，因此某些人为构造的“非正式别名”可能曾经碰巧指向正式 key。新实现会拒绝这些别名。

正式上传响应返回的 key 不受影响。

#### 迁移步骤

1. 从访问日志或业务数据库收集真实使用的 `/file/download/:key` 样本。
2. 对每个 key 运行新解析器的离线 `--check-key`。
3. 不通过但仍需保留的 key，替换为同一文件的正式 canonical key。
4. 只有样本全部通过后才能上线严格解析。

#### 验证

- `a-x`、`-xx`、16 位大写 hash、短 hash、超长 key 均返回 400。
- canonical key 正常下载。
- `FuzzExtractLinkFromFileKey` 任意输入永不 panic。
- 公开接口连续发送畸形 key 不产生 panic 堆栈日志。

### C-04 修正 `/defauls/` 并迁移历史映射

#### 代码修改

```go
const defaultUploadPrefix = "/defaults/"
```

删除代码中 `/defauls/` 常量。`buildFileKeyLink`、`extractLinkFromFileKey`、FileDownload 和 GetMetaInfo 只使用 `/defaults/`，不增加 fallback。

外部 URL 中只包含 `<key>`，不包含 `/defauls/`；停服迁移根目录名后，相同 key 会定位到同一 mapping 和 file_id，因此外部直链不需要改写。

#### 离线可逆迁移命令

执行：

```shell
tgfile migrate-default-prefix \
  --config=/config/config.json \
  --direction=forward \
  --dry-run=true
```

支持：

```text
forward: /defauls -> /defaults
reverse: /defaults -> /defauls
```

该命令是离线维护命令：

1. 不启动 HTTP、Telegram、缓存和任何上传逻辑。
2. `dry-run=true` 以 SQLite 只读模式打开数据库。
3. `dry-run=false` 使用 `BEGIN IMMEDIATE` 单事务执行。
4. 不调用 `db.InitDB`，避免顺带建表或变更 schema。
5. 直接定位根节点 `/` 下的一级目录，只允许目录类型 `file_kind=1`。

执行矩阵：

| 源目录 | 目标目录 | 行为 |
|---|---|---|
| 不存在 | 不存在 | 拒绝，退出 2 |
| 不存在 | 存在 | 已迁移，dry-run 退出 0；正式执行拒绝，退出 2 |
| 存在 | 不存在 | 在一个 SQLite 事务中 Move，overwrite=false |
| 存在 | 存在 | 拒绝，退出 2，不合并、不覆盖 |

正式事务固定执行：

1. 确认根节点 `/` 恰好一条。
2. 确认源目录恰好一条、目标目录为零条。
3. 记录源目录 `entry_id`、`parent_entry_id`、`ctime`、`mtime`、子节点数。
4. 只执行一条根目录重命名 UPDATE。
5. 要求 `RowsAffected == 1`。
6. 在提交前再次确认源为零、目标为一，且 `entry_id` 和子节点数未变。
7. 任一条件不满足立即 ROLLBACK。

#### 历史数据变化

forward 会修改目录根节点：

```text
file_name: defauls -> defaults
```

迁移命令不得修改该行的 `mtime`、`ctime`、`entry_id`、`parent_entry_id` 或其他字段。

不会修改：

- 子节点；
- file_id；
- ref_data；
- tg_file_tab；
- tg_file_part_tab；
- Telegram/localfile FileKey；
- 外部下载 key。

#### 正向迁移

1. 关闭上传入口。
2. 停止旧服务并确认容器/进程已经退出。
3. 保存 SQLite 备份、数据库校验结果和 `audit-before.json`。
4. 使用新镜像的一次性维护命令执行 forward dry-run。
5. 只有“`/defauls` 恰好一条、`/defaults` 为零条”时执行 forward。
6. 再次运行 audit，确认只有根目录名发生变化。
7. 更新服务镜像并启动新版本。
8. 对迁移前样本执行完整或 Range hash，对比必须一致。

#### 回滚

回滚旧镜像前必须：

1. 停止新服务。
2. 使用新镜像的离线命令执行 reverse dry-run。
3. 确认 `/defauls` 不存在、`/defaults` 存在。
4. 执行 reverse。
5. 审计数据库；若 reverse 失败，恢复停服时的 SQLite 备份。
6. 切回旧镜像并启动。
7. 验证样本。

如果两个目录同时存在，不允许启动任一版本，必须保持停服并人工处理。

#### 验证

构造 `/defauls/` 文件后：

1. 记录外部 URL、file_id、所有分片 FileKey、内容 hash。
2. forward 后全部值除根目录名外保持不变。
3. 使用只认识 `/defaults/` 的新代码验证原外部 URL 内容 hash 不变。
4. reverse 后使用旧代码验证再次一致。
5. 双目录冲突时数据库零变化。

### C-05 使用 Cobra 拆分命令

原入口把服务启动、审计、路径迁移和 key 校验参数注册在同一个 flag 集合中，再通过
字符串分支选择操作。改造后根命令不承载业务参数，固定提供：

```text
tgfile serve
tgfile audit
tgfile migrate-default-prefix
tgfile check-key
```

#### 参数归属

- `serve`：只接受 `--config`；
- `audit`：只接受 `--config`、`--output`；
- `migrate-default-prefix`：只接受 `--config`、`--direction`、`--dry-run`；
- `check-key`：只接受 `--key`。

根命令未指定子命令时返回退出码 2。参数解析错误、缺少必填参数、迁移前置条件不满足
以及非法 key 返回退出码 2；运行期 I/O 或数据库错误返回退出码 1。

#### 部署和开发入口

容器保持 `/bin/tgfile` 为 ENTRYPOINT，并将 `serve` 设为默认 CMD。Compose 显式使用：

```yaml
command: serve --config=/config/config.json
```

开发脚本使用：

```shell
go run ./cmd serve --config=/path/to/config.json
```

离线维护命令覆盖容器默认 CMD 后可以直接执行，不初始化 HTTP 服务、Telegram 或缓存。

#### 验证

- 每个子命令的 `--help` 只展示自身参数；
- `audit --key=value` 等跨命令参数返回 unknown flag；
- 根命令不带子命令时拒绝执行；
- audit、forward dry-run、正式迁移和 key 校验命令测试通过；
- Docker 默认启动和 `make dev` 均显式进入 `serve`。

### C-06 加固 S3 PUT 共用的 CreateFile

主要代码：

- `filemgr/file_manager_impl.go`
- `server/handler/s3/object.go`
- `server/handler/file/file_upload.go`

#### CreateFile 固定行为

1. `size < 0`：上传前返回参数错误，不创建 Draft。
2. `bkio.MaxFileSize() <= 0`：返回配置错误。
3. 使用无溢出公式计算分片数：

```text
size == 0: block_count = 0
size > 0:  block_count = 1 + (size - 1) / block_size
```

4. 固定最大分片数为 100,000；超过时上传前返回错误。
5. 每片期望值：

```text
part_size = min(block_size, size - uploaded_size)
```

6. 每片 Reader 使用 `io.LimitReader(reader, part_size)`，不能对末片使用完整 block_size。
7. 使用 counting Reader 记录 BlockIO 实际消费的字节数。
8. BlockIO 返回成功后，要求 `actual == part_size`；短读返回错误。
9. 所有分片成功后才调用 FinishFileCreate。
10. 零字节文件继续允许 0 分片。

本轮不新增 `file_part_size` 数据库列；CreateFile 在单个请求内可以验证实际读取字节数，
不需要对历史数据回填。

#### HTTP/S3 输入

- S3 PUT 使用中间件处理后的 `Request.ContentLength`。
- ContentLength 仍为 -1 时直接返回 411/400，不能转换为 uint64。
- `/file/upload` 使用 `multipart.FileHeader.Size`。
- 备份导入虽然不是核心功能，但会自动获得同一短读保护。

#### 失败后的数据

短读或后端失败可能留下无引用 Draft/分片，但：

- 不创建 S3/直链映射；
- 不修改任何既有映射；
- 不覆盖既有对象；
- C-01 会报告其为 unreferenced。

本轮不自动删除这些记录。

#### 验证

使用内存 BlockIO、block_size=4：

| 声明大小 | Reader 大小 | 预期 |
|---:|---:|---|
| -1 | 任意 | 上传前失败 |
| 0 | 0 | 成功，0 分片 |
| 1 | 1 | 成功，1 分片 |
| 5 | 5 | 成功，分片 4+1 |
| 5 | 4 | 失败，无映射 |
| 8 | 8 | 成功，分片 4+4 |

额外验证：

- 多块 S3 PUT 后完整 GET hash 一致。
- HEAD 的 Content-Length 正确。
- Range 跨分片边界内容正确。
- 失败请求不改变原有对象。

### C-07 S3 PUT 冲突和孤儿审计

当前 S3 PUT 对已存在路径先上传、后创建 mapping，冲突时会产生一个无引用新文件。本轮不启用覆盖，但要消除无意义上传。

#### 实现

在 `UploadObject` 上传前：

1. 先修正 `filemgr.StatFileLink`：找不到 mapping 时返回包装后的 `os.ErrNotExist`，禁止返回无法分类的普通字符串错误。
2. `UploadObject` 先调用 `StatFileLink(filename)`。
3. 已存在文件或目录时直接返回 409，不调用 CreateFile。
4. 只有 `errors.Is(err, os.ErrNotExist)` 时才继续上传。
5. 数据库查询错误返回 500。
6. 上传结束到创建 mapping 之间仍可能发生并发竞争；CreateFileLink 返回 `os.ErrExist` 时返回 409，新 file_id 留作审计，不覆盖目标。

这不是强一致覆盖实现，但可以避免正常重复 PUT 产生后端孤儿。

#### 并发限制

同一路径的单进程并发 PUT 使用 keyed mutex 串行化：

1. key 为规范化 S3 object path。
2. 获取锁后再次 Stat。
3. 请求结束释放并删除无引用锁条目。

多实例部署仍依赖 SQLite UNIQUE 约束做最终防线。

#### 验证

- 对已存在对象 PUT：返回 409，BlockIO Upload 调用次数为 0，原内容不变。
- 两个并发 PUT 同一路径：一个成功、一个 409，最终只有一个 mapping。
- 两个不同路径可并发上传。

### C-08 Telegram 超时和取消

#### HTTP Client

使用 `tgbotapi.NewBotAPIWithClient` 注入：

```text
Dial timeout              10s
TLS handshake timeout     10s
Response header timeout  120s
Idle connection timeout  120s
Overall request timeout   30min
```

现有配置不增加必填字段；以上作为缺省值。

#### context

Telegram 库的 Send 不接收 context，因此上传 Reader 外再包一层：

```text
每次 Read 前：
  ctx.Done -> 返回 ctx.Err
  default  -> 读取原 Reader
```

请求取消后 multipart 读取失败，底层 HTTP 请求随之结束。整体 Client Timeout 是最终兜底。

下载继续使用 `NewRequestWithContext`。

上传不是幂等操作：Telegram 已接收文件但响应丢失时，自动重试会生成重复消息。因此本轮不在 BlockIO 内自动重试上传；失败返回上层，由整个 S3 PUT 重新发起。

下载和只读 GetFile 请求允许重试，范围固定为网络临时错误、HTTP 429、500、502、503、504；其他 4xx 不重试。

#### 数据影响

不会修改存量块。超时只会让当前新上传或下载失败，不会删除数据。

#### 验证

- Telegram 模拟服务不返回响应，按配置超时。
- context 取消后 Reader 和请求退出。
- 已有文件下载内容不变。
- 上传错误不在 BlockIO 内自动重试。
- 下载/GetFile 的 429/5xx 会重试，404 不重试。

### C-09 HTTP 优雅关闭

#### 实现

不再使用 Gin Engine 自带 Run，使用显式 `http.Server`：

```text
ReadHeaderTimeout  10s
IdleTimeout       120s
MaxHeaderBytes      1MiB
ReadTimeout         0
WriteTimeout        0
```

ReadTimeout、WriteTimeout 保持 0，避免大文件存量读取被统一时限截断。

进程使用 `signal.NotifyContext`：

1. SIGINT/SIGTERM 后停止接受新连接。
2. 最多等待 30 秒。
3. 调用 HTTP Server Shutdown。
4. 关闭 SQLite。
5. `http.ErrServerClosed` 视为正常退出。

#### 验证

- 大文件完整下载不被 WriteTimeout 截断。
- SIGTERM 后已有短下载完成，新连接被拒绝。
- 退出后 SQLite `PRAGMA quick_check=ok`。

### C-10 缓存一致性

#### L1

保留 Ristretto：

1. `Set` 返回 false 时返回 `ErrCacheSetRejected`。
2. Set 返回 true 后调用 `Cache.Wait()`。
3. Wait 后 Get，仍不存在则按 reject 处理。
4. reject 只影响命中率；当前请求仍返回已读到的字节。

#### L2

同样等待 Set 生效，并明确缓存文件所有权：

1. 文件安全落盘后调用 Set。
2. Set 成功且 Wait 后可见，文件由 L2 cache 接管。
3. Set 失败或策略 reject，文件不进入长期缓存；当前 Reader 关闭后删除。
4. `OnReject`、`OnEvict` 都记录 file_id、size 和删除结果。
5. 删除失败只记录缓存错误，不删除 SQLite/Telegram/localfile 数据。

#### 历史缓存迁移

不需要保留历史 L2 缓存。

上线时可以：

1. 停止服务。
2. 将现有 cache 目录改名为 `cache.pre-upgrade`，不要直接删除。
3. 启动新版本并从后端回源验证。
4. 观察一周后再清理旧 cache 目录。

如果新缓存代码异常：

1. 关闭 L1/L2 cache 配置。
2. 重启并直接从后端读取。
3. 原始文件不受影响。

#### 验证

- Set 返回成功后立即 Get 可见。
- reject 后没有永久遗留的未登记 cache 文件。
- 完全禁用/移走 cache 后，存量样本 hash 一致。
- `go test -race` 下并发读无路径删除竞态。

### C-11 S3/直链测试和 CI

#### 测试隔离

1. 数据库和缓存全部使用 `t.TempDir()`。
2. 不再使用固定 `/tmp/*.db`。
3. 删除依赖 `127.0.0.1:9901` 现有服务的默认测试。
4. 使用 `httptest.Server`、临时 SQLite、mem BlockIO 构建完整测试服务。
5. 后续还要使用返回值时，先 `require.NoError`，不能 assert 后继续导致二次 panic。

#### 核心回归

必须覆盖：

1. S3 PUT -> GET -> HEAD。
2. S3 多块文件和跨块 Range。
3. S3 已存在路径 PUT 不上传、不覆盖。
4. `/file/upload` -> `/file/download/:key`。
5. canonical key、畸形 key 和 key fuzz。
6. `/defauls/` 离线 forward、reverse，以及新旧代码与对应路径的配对验证。
7. 禁用缓存后的后端读取。
8. Telegram 超时和取消。
9. Cobra 子命令参数隔离和退出码符合 C-05。

#### PR CI

新增 PR/主分支 workflow，固定使用无已知可达标准库漏洞的 Go 1.25.12：

```shell
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./...
govulncheck ./...
```

调整 tag release workflow：

1. 更新 `actions/checkout`、`actions/setup-go` 到当前受支持版本。
2. Go 版本与 `go.mod` 一致。
3. 只构建 tgfile。
4. 只有测试 workflow 成功后才允许发布。

## 5. 本轮不实施的原审查项

由于线上主要使用 S3 和直链，下列改动延后，避免扩大一次上线的回归面：

1. WebDAV COPY/MOVE/PUT 语义重构。
2. 备份格式 manifest 和原子导入。
3. S3 自动覆盖已存在对象。
4. 后端 Delete 和真实 GC。
5. 历史 MD5 重算。
6. 公有/私有读取模式调整。
7. 目录表外键和完整性约束。
8. 容器非 root 与卷权限迁移。

仍可做而且完全独立的低风险修复是备份导出循环内及时 Close 文件，但不作为核心上线阻塞项。

## 6. 历史异常处理原则

C-01 如果发现历史异常，不允许在本轮自动修复，按以下规则输出迁移清单。

### 6.1 mapping 指向不存在 file_id

影响：该路径已经无法正常读取。

步骤：

1. 导出路径、entry_id、ref_data、ctime、mtime。
2. 从备份数据库或外部备份确认是否能恢复 file 元数据。
3. 能恢复：先在隔离库恢复并验证下载，再生成单独 SQL。
4. 不能恢复：保留路径至少一个观察周期；获得人工确认后才删除 mapping。

### 6.2 mapping 指向非 Ready file

不能直接把状态改成 Ready。

步骤：

1. 核对声明分片数和实际分片编号。
2. 在隔离环境完整读取并计算 hash。
3. 能完整读取且业务确认内容正确，才允许单条迁移为 Ready。
4. 缺片或读取失败，保持原状，不创建新映射。

### 6.3 Ready 文件分片数不符

新读取代码仍按历史行为处理，不因审计结果拒绝读取。

步骤：

1. 对该文件做完整下载验证。
2. 实际可读且 hash 符合业务记录：修正声明分片数前先备份对应行。
3. 不可读：从备份恢复，不能通过猜测补分片。

### 6.4 无引用 file_id

本轮只报告，不删除。

未来清理必须：

1. 生成候选清单。
2. 至少观察 30 天。
3. 再次确认没有任何 mapping。
4. 先 tombstone，再删除后端块。
5. 后端成功后才删除元数据。

## 7. `/defauls/` 专项迁移步骤

这是本轮唯一主动修改历史业务行的操作。服务停止后执行，整个窗口内不允许任何 tgfile 实例连接生产数据库。

### 7.1 迁移前固定快照

保存以下查询结果：

```sql
PRAGMA quick_check;
SELECT file_state, COUNT(*), COALESCE(SUM(file_size), 0)
FROM tg_file_tab GROUP BY file_state;
SELECT COUNT(*) FROM tg_file_part_tab;
SELECT COUNT(*) FROM tg_file_mapping_tab;

SELECT entry_id, parent_entry_id, file_kind, file_name, ctime, mtime
FROM tg_file_mapping_tab
WHERE parent_entry_id = (
    SELECT entry_id
    FROM tg_file_mapping_tab
    WHERE parent_entry_id = 0 AND file_name = '/'
)
AND file_name IN ('defauls', 'defaults');
```

同时执行 audit 并保存 JSON，抽取 `/defauls/` 下所有 mapping 的：

```text
entry_id
parent_entry_id
file_name
ref_data
file_size
```

直链样本至少 20 个；不足 20 个则全量。停服前保存小文件完整 SHA-256，大文件保存文件长度、首尾各 1 MiB Range 的 SHA-256。

### 7.2 执行 forward

```shell
tgfile migrate-default-prefix \
  --config=/config/config.json \
  --direction=forward \
  --dry-run=true
```

仅当：

```text
root_count=1
source_count=1
target_count=0
source_kind=1
source_is_directory=true
```

时执行：

```shell
tgfile migrate-default-prefix \
  --config=/config/config.json \
  --direction=forward \
  --dry-run=false
```

命令必须输出 `changed_rows=1` 和迁移前后的同一个 `entry_id`；否则视为失败。

### 7.3 迁移后

1. `PRAGMA quick_check=ok`。
2. file/part/mapping 总数与迁移前一致。
3. `/defaults` 存在，`/defauls` 不存在。
4. 根目录行除 `file_name` 外所有列与迁移前一致。
5. file_id、ref_data、FileKey 不变。
6. audit 历史异常数量不增加。

此时服务仍然保持停止。任何一项失败都不得更新服务镜像，立即 reverse；reverse 失败则恢复 SQLite 备份。

## 8. 单次停服上线操作清单

所有功能在一次停服窗口上线。下面的检查项必须逐项签字或记录命令输出，不允许跳项。

### 8.1 窗口前准备，不停服

1. 生成不可变的新镜像地址，必须使用 digest，禁止使用 `latest`。
2. 完成 C-11 自动化测试和第 10 节中可在发布前完成的验收项，并保存 CI 链接。
3. 提前拉取新镜像，但不启动、不替换当前服务。
4. 准备旧镜像 digest，确认本机或镜像仓库仍可拉取。
5. 安装并验证 `sqlite3`、`sha256sum`、`curl`、AWS CLI。
6. 确认数据库、配置、Compose 文件的绝对路径和可用磁盘空间。
7. 验证备份盘剩余空间至少为数据库文件及 WAL 总大小的 2 倍。
8. 查询 `file_state=1` 的 Draft 清单并归档，不删除。
9. 选取至少 20 个存量直链和 20 个 S3 对象；不足时全量。
10. 记录直链/S3 样本的 Content-Length、ETag/MD5（存在时）、完整或 Range SHA-256。
11. 备份当前配置和部署文件；准备包含新 Token/密码的正式配置，以及旧镜像可读取、使用相同新凭据的回滚配置。
12. 明确上线负责人、验证人、回滚负责人和最长停服时间。

执行前设置并人工复核以下变量；路径必须为绝对路径：

```shell
export TGFILE_COMPOSE_FILE=/opt/tgfile/docker-compose.yml
export TGFILE_SERVICE=tgfile
export TGFILE_CONFIG_DIR=/opt/tgfile/config
export TGFILE_DATA_DIR=/opt/tgfile/data
export TGFILE_DB=/opt/tgfile/data/data.db
export TGFILE_BACKUP_DIR=/opt/tgfile/backups/release-YYYYMMDD-HHMM
export TGFILE_OLD_IMAGE='registry.example/tgfile@sha256:OLD_DIGEST'
export TGFILE_NEW_IMAGE='registry.example/tgfile@sha256:NEW_DIGEST'
```

如果 L2 缓存启用且目录通过宿主机持久化挂载，再设置：

```shell
export TGFILE_L2_CACHE_DIR=/absolute/host/path/to/tgfile-cache
export TGFILE_L2_CACHE_BACKUP=/absolute/host/path/to/tgfile-cache.pre-upgrade-YYYYMMDD-HHMM
```

执行保护检查：

```shell
test -f "$TGFILE_COMPOSE_FILE"
test -f "$TGFILE_CONFIG_DIR/config.json"
test -f "$TGFILE_DB"
test "$TGFILE_OLD_IMAGE" != "$TGFILE_NEW_IMAGE"
docker image inspect "$TGFILE_OLD_IMAGE" >/dev/null
docker image inspect "$TGFILE_NEW_IMAGE" >/dev/null
```

上述任一命令失败则取消上线。

### 8.2 停服和备份

1. 在网关关闭 S3 PUT 和 `/file/upload`。
2. 停止服务：

```shell
docker compose -f "$TGFILE_COMPOSE_FILE" stop "$TGFILE_SERVICE"
```

3. 确认 tgfile 容器已经停止、端口不再监听，并确认没有其他实例挂载同一个数据库。
4. 创建新的空备份目录，不复用旧目录：

```shell
install -d -m 0700 "$TGFILE_BACKUP_DIR"
```

5. 停服后执行 WAL checkpoint 和 SQLite 在线备份：

```shell
sqlite3 "$TGFILE_DB" "PRAGMA wal_checkpoint(FULL);"
sqlite3 "$TGFILE_DB" ".backup '$TGFILE_BACKUP_DIR/data.db'"
sqlite3 "$TGFILE_BACKUP_DIR/data.db" "PRAGMA quick_check;"
sha256sum "$TGFILE_BACKUP_DIR/data.db" >"$TGFILE_BACKUP_DIR/data.db.sha256"
cp --preserve=mode,timestamps "$TGFILE_CONFIG_DIR/config.json" "$TGFILE_BACKUP_DIR/config.json"
cp --preserve=mode,timestamps "$TGFILE_COMPOSE_FILE" "$TGFILE_BACKUP_DIR/docker-compose.yml"
```

`quick_check` 输出不是 `ok` 时立即取消上线，不执行迁移。

6. 使用新镜像的 `audit` 子命令保存只读审计：

```shell
docker run --rm \
  -v "$TGFILE_CONFIG_DIR:/config:ro" \
  -v "$TGFILE_DATA_DIR:/data" \
  -v "$TGFILE_BACKUP_DIR:/maintenance" \
  "$TGFILE_NEW_IMAGE" \
  audit \
  --config=/config/config.json \
  --output=/maintenance/audit-before.json
```

7. 按第 7.1 节保存表计数和 `/defauls` 根目录行。确认：

```text
quick_check=ok
root_count=1
source_count=1
target_count=0
```

任何条件不满足均保持停服并取消上线。

8. 处理 L2 缓存：

- L2 未启用：不操作。
- L2 位于容器临时文件系统：不保留，随旧容器销毁。
- L2 通过宿主机持久化：确认 `$TGFILE_L2_CACHE_DIR` 存在、`$TGFILE_L2_CACHE_BACKUP` 不存在，将原目录重命名为备份目录，再以相同 owner、group、mode 创建空目录。

持久化目录执行：

```shell
test -d "$TGFILE_L2_CACHE_DIR"
test ! -e "$TGFILE_L2_CACHE_BACKUP"
mv -- "$TGFILE_L2_CACHE_DIR" "$TGFILE_L2_CACHE_BACKUP"
mkdir -- "$TGFILE_L2_CACHE_DIR"
chown --reference="$TGFILE_L2_CACHE_BACKUP" "$TGFILE_L2_CACHE_DIR"
chmod --reference="$TGFILE_L2_CACHE_BACKUP" "$TGFILE_L2_CACHE_DIR"
```

L2 缓存只包含可重建副本，不能把它混入 SQLite 业务数据备份。

### 8.3 离线路径迁移

1. 执行 dry-run：

```shell
docker run --rm \
  -v "$TGFILE_CONFIG_DIR:/config:ro" \
  -v "$TGFILE_DATA_DIR:/data" \
  "$TGFILE_NEW_IMAGE" \
  migrate-default-prefix \
  --config=/config/config.json \
  --direction=forward \
  --dry-run=true
```

2. 输出满足第 7.2 节的四个前置条件后执行正式迁移：

```shell
docker run --rm \
  -v "$TGFILE_CONFIG_DIR:/config:ro" \
  -v "$TGFILE_DATA_DIR:/data" \
  "$TGFILE_NEW_IMAGE" \
  migrate-default-prefix \
  --config=/config/config.json \
  --direction=forward \
  --dry-run=false
```

3. 再次执行 `PRAGMA quick_check` 和第 7.3 节的计数对比，并保存迁移后 audit：

```shell
docker run --rm \
  -v "$TGFILE_CONFIG_DIR:/config:ro" \
  -v "$TGFILE_DATA_DIR:/data" \
  -v "$TGFILE_BACKUP_DIR:/maintenance" \
  "$TGFILE_NEW_IMAGE" \
  audit \
  --config=/config/config.json \
  --output=/maintenance/audit-after.json
```

4. 只有所有结果一致，才允许进入启动步骤。

### 8.4 更新镜像并一次启动

1. 将服务部署文件中的 tgfile 镜像替换为 `$TGFILE_NEW_IMAGE`，不得改用浮动 tag。
2. 轮换 Telegram Bot Token 和上传账号密码，将新凭据同时写入正式配置与回滚配置；配置 schema 保持不变。
3. 通过 Telegram 官方接口验证新 Token 属于原 Bot；禁止把 Token 输出到终端或日志。
4. 除凭据轮换外不调整其他业务配置；日志脱敏由新代码实现。
5. 不分批启用功能。
6. 启动唯一实例：

```shell
docker compose -f "$TGFILE_COMPOSE_FILE" up -d --no-deps "$TGFILE_SERVICE"
```

7. 确认实际运行镜像 digest 等于 `$TGFILE_NEW_IMAGE`，且只有一个实例。
8. 检查启动日志中没有 Token、密码、panic、数据库错误或缓存初始化错误。

### 8.5 启动后验证

按固定顺序验证：

1. 全部直链样本返回 200/206，Content-Length 和 SHA-256 与窗口前一致。
2. 全部 S3 样本 GET、HEAD、Range 返回值和内容 hash 与窗口前一致。
3. 对一个全新、带发布编号的 S3 key 执行小文件 PUT -> HEAD -> GET，内容 hash 一致。
4. 对该已存在 key 再次 PUT，必须返回 409，原内容不变。
5. 畸形直链 key 返回 400，服务无 panic。
6. 执行 audit，`quick_check=ok`，历史异常数量不增加。
7. 检查 Telegram、HTTP、缓存错误率和进程资源。
8. 网关重新开放 S3 PUT。
9. 归档必要的上线日志后，按日志系统保留策略清理升级前可能含旧 Token/密码的历史日志。

生产 S3 冒烟对象不自动删除，记录其 key、file_id 和 hash，后续纳入明确的 GC 流程。

## 9. 整体回滚

出现以下任一情况立即回滚：

- 新服务无法启动或反复重启；
- 任一存量样本内容、长度或 Range 不一致；
- `PRAGMA quick_check` 非 `ok`；
- audit 历史异常数量增加；
- S3 GET/HEAD 或直链出现持续 5xx；
- 发现两个根目录或路径迁移结果不唯一。

回滚顺序固定为：

1. 关闭网关上传入口并停止新服务。
2. 使用 `$TGFILE_NEW_IMAGE` 执行 reverse dry-run。
3. 仅当 `source=/defaults` 恰好一条且 `target=/defauls` 为零条时执行 reverse。
4. reverse 后执行 `PRAGMA quick_check` 和根目录检查。
5. reverse 失败或结果不一致时，不再尝试修补；先执行 SQLite `.backup` 将当前数据库保存为故障副本，再执行 `.restore` 从 `$TGFILE_BACKUP_DIR/data.db` 恢复，并确认 `PRAGMA quick_check=ok`。
6. 恢复旧部署文件，镜像固定为 `$TGFILE_OLD_IMAGE`；配置使用窗口前准备的回滚配置，其中必须是已经轮换后的新凭据，不能恢复已失效的旧 Token。
7. 启动旧服务。
8. 验证同一批直链和 S3 样本。
9. 验证成功后才重新开放上传。

禁止将旧镜像直接连接已迁移为 `/defaults` 的数据库，也禁止让新镜像连接已恢复为 `/defauls` 的数据库。

备份恢复命令固定为：

```shell
sqlite3 "$TGFILE_DB" ".backup '$TGFILE_BACKUP_DIR/data.failed.db'"
sqlite3 "$TGFILE_DB" ".restore '$TGFILE_BACKUP_DIR/data.db'"
sqlite3 "$TGFILE_DB" "PRAGMA quick_check;"
```

如果上线前移走了持久化 L2 缓存，回滚时优先保持缓存禁用或使用空目录；数据库和后端块可独立完成读取验证后，才允许恢复旧缓存目录。

## 10. 最终验收标准

必须全部满足：

1. 上线前后 `PRAGMA quick_check=ok`。
2. 除显式 `/defauls/` 根目录迁移外，存量业务行不变。
3. 存量 file_id、FileKey、MD5、ref_data 不变。
4. S3 PUT/GET/HEAD、多块和 Range 测试通过。
5. 直链 canonical key 正常，畸形 key 永不 panic。
6. 停服执行 `/defauls/` forward/reverse 后，原 URL 和内容 hash 不变。
7. Cobra 子命令参数隔离、帮助和退出码验证通过。
8. S3 已存在路径 PUT 不调用 BlockIO、不覆盖内容。
9. 短读不能生成 Ready 文件映射。
10. 禁用或清空缓存后存量文件仍可读取。
11. audit 中的历史异常数量不增加。
12. `go vet ./...`、`go test ./...`、`go test -race ./...`、`govulncheck ./...` 全部通过。
13. 整个改造没有执行任何后端块删除。

## 11. 推荐 PR 拆分

1. PR-1：C-01、C-02、C-03、测试基础。
2. PR-2：C-05 Cobra 子命令拆分、Docker/开发入口和命令测试。
3. PR-3：C-06、C-07，完成 S3 上传核心加固。
4. PR-4：C-08、C-09。
5. PR-5：C-10。
6. PR-6：C-04 单路径切换、离线可逆迁移命令和上线操作手册。

PR 可以拆分开发和评审，但生产环境只发布包含全部 PR 的一个镜像，并按第 8 节一次停服上线。除停服窗口内显式执行的根目录重命名外，不应改变任何存量文件映射或文件块。
