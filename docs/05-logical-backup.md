# 逻辑备份格式与恢复模型

本文定义 tgfile 逻辑备份 v2 的稳定格式、任务状态机、恢复事务和管理接口。系统组件关系见
[`01-architecture.md`](01-architecture.md)，持久化表见
[`02-data-and-storage-model.md`](02-data-and-storage-model.md)，S3 与直链语义见
[`03-core-flows-and-api.md`](03-core-flows-and-api.md)。

## 1. 能力边界

逻辑备份用于把当前可见的路径树、文件内容和稳定协议元数据迁移到新的 tgfile 数据库及
BlockIO 后端。它保证：

- Mapping 绝对路径保持不变，因此 S3 bucket/key、WebDAV 路径和直链 key 保持不变；
- 同一 File 的多个 Mapping 在恢复后仍共享同一个新 File；
- layout v1 的物理 Part 边界、大小和 MD5 保持不变；
- layout v2 的 Segment 顺序、Completed Part manifest、S3 ETag、additional checksum
  和对象元数据保持不变；
- WebDAV dead property 保持不变；
- 导入生成新的 FileID、EntryID、FileKey 和 DeleteRef，发布前内容对外不可见；
- 所有 Mapping、S3 Metadata、WebDAV Property、change journal 和 Job 成功状态在一个
  SQLite 事务中发布。

归档不包含凭据、缓存、活动 Multipart Upload、WebDAV Lock、历史 change journal、
删除任务或 backup Job。SQLite、配置和 Telegram 原消息的原样备份仍是最高保真的灾备
手段；逻辑备份不能突破 Telegram Bot API 对消息删除时间窗口的限制。

## 2. 包与依赖

| 组件 | 职责 |
|---|---|
| `backupfmt` | 版本化归档模型、严格 JSON、tar/gzip 读取、摘要和资源限制 |
| `backupmgr` | 持久化 Job、幂等、异步执行、恢复、取消、报告、清理和指标 |
| `filemgr` | snapshot、Export Pin、精确 Part restore、Composite 重建和原子发布 |
| `server/handler/backup` | Basic Auth、backup 角色、HTTP 参数和 artifact 响应 |
| `server/handler/admin` | 管理 Session、管理角色、Job 列表和浏览器上传下载 |
| `cmd` | `backup export`、`backup verify`、`backup import` 离线入口 |
| `maintenance` | Job、Pin、staged File、DeleteRef 和 work dir 的只读审计 |

HTTP handler 和 `backupmgr` 不直接修改文件、Mapping 或协议元数据表；这些操作都通过
FileManager 的备份接口完成。`backupfmt` 不依赖数据库、HTTP 或 BlockIO，因此 Verify
可以离线执行。

## 3. `.tgfb` 归档

媒体类型为 `application/vnd.tgfile.backup.v2+tar+gzip`，扩展名为 `.tgfb`。归档是单个
gzip member 内的 POSIX tar，条目顺序固定：

```text
format.json
parts/f00000001/00000000.bin
parts/f00000001/00000001.bin
...
manifest.json
```

`format.json` 固定声明：

```json
{"format":"tgfile-logical-backup","version":2}
```

Manifest 使用 `f` 加八位十进制数字作为归档内 File 引用，并保存：

- source schema、BlockIO 类型和单 Part 上限；
- 归档使用的 bucket 名称及 `private`/`public-read` ACL；
- File layout、大小、兼容性 MD5、物理 Part、Composite Segment 和 Completed Part；
- Directory 与 Mapping 的路径、mode、ctime、mtime；
- S3 ETag、对象 header、用户元数据和 checksum 三元组；
- WebDAV dead property 的路径、namespace、local name、XML 值和时间；
- Mapping、Directory、File、Part 与物理字节汇总。

每个物理 Part 保存精确 size、MD5、SHA-256 和唯一 tar entry。零字节 File 没有 Part，
兼容性 MD5 固定为 `d41d8cd98f00b204e9800998ecf8427e`。layout v2 不重复保存内容，
只引用归档中的 layout v1 source File。

解析器拒绝未知或重复 JSON 字段、非法 UTF-8、多个 JSON 值、非普通 tar 条目、PAX 扩展、
重复或未声明条目、路径穿越、多个 gzip member和尾随数据。File、路径和协议元数据数组
必须使用规范排序；Part/Segment 编号必须连续；所有引用、大小、摘要、checksum 组合和
Manifest 汇总必须自洽。验证同时使用配置限制和实际读取计数，不能信任归档声明值。
Verify 还会按物理 File 连续读取内容，重新计算五种 S3 checksum；Completed Part checksum
必须与对应 source File 字节一致，最终对象的 FULL_OBJECT/COMPOSITE checksum 也必须能由
Completed Part 重建，不能只满足 Base64 长度。

整个压缩 artifact 另算小写 SHA-256。它存入 Job，作为 artifact HTTP ETag 和
`X-Tgfile-Artifact-SHA256`，不写入归档内部。

## 4. Job 与状态机

Export 和 Import 使用 `tg_backup_job_tab`，幂等范围是
`(owner, job_kind, idempotency_key)`。相同 key 和相同参数返回原 Job，不同参数返回冲突。
Job ID 是 32 字节密码学随机数的小写十六进制文本。

状态集合：

```text
receiving
queued
snapshotting
building
validating
staging
publishing
canceling
succeeded
failed
canceled
```

后三个状态为终态。状态转换以条件更新领取 queued Job，单个数据库最多同时运行一个
Export 和一个 Import。一个 SQLite 数据库只允许由一个 tgfile 服务进程运行 backup
恢复器；不能让两个服务实例共享同一 DB，因为第二个实例无法区分崩溃遗留 Job 和另一个
仍在执行的 Job。取消请求持久化到数据库；worker 在 Part 边界检查取消状态。进入
`publishing` 后拒绝取消，因为发布是短 SQLite 事务。

Import 的每个归档 File 在 `tg_backup_job_file_tab` 中保存新 FileID、layout、stage state
和下一个 Part 游标。Export 在 snapshot 事务内把 final/source File 写入
`tg_backup_export_pin_tab`；引用判断把 Pin 当作临时有效引用，避免导出期间的覆盖或删除
使源 Part 进入删除队列。

失败 Job 只向 API 返回固定低基数错误 code 和脱敏摘要。权限为 `0600` 的 report 文件
保存在 work dir，并由 Job 的相对 `report_path` 引用。错误 code 包括归档结构/摘要/限制、
空间不足、源不可读、目标不兼容、路径冲突、后端上传/回读失败、发布失败和内部错误。

## 5. Export

Export 先在一个短 SQLite 事务中解析 scope：

1. 读取 scope 内 Mapping、Directory 和必需的父目录；
2. 递归读取所有 layout v2 source File；
3. 读取 Part、Segment、Completed Part、S3 Metadata 和 WebDAV Property；
4. 为缺少 S3 Metadata 的历史对象实体化当前兼容元数据；
5. 拒绝非 ready File，以及缺失、非 live 或不可删除引用不完整的物理 Part；
6. 为全部 final/source File 建立 Export Pin。

随后 worker 把 snapshot 写入 `0700` work dir，在构建 `.partial` artifact 时逐 Part 从
BlockIO 下载。写入过程重新计算 size、MD5 和 SHA-256；任何短读、长读、摘要变化或后端
错误都会使 Job 失败。完成 tar/gzip、fsync、离线 Verify 和 artifact SHA-256 后，文件通过
原子 rename 发布。Job 成功和 Pin 删除处于同一事务；snapshot 随后不再保留。

失败或取消会删除 partial、snapshot、未登记 artifact 并释放 Pin，不修改 File、Mapping
或协议业务表。

## 6. Import

HTTP Import 请求体直接是 `.tgfb`。接收阶段要求精确 Content-Length、artifact SHA-256 和
Idempotency-Key；请求体流式写入 `0600` partial，同时计算摘要，fsync 后原子 rename。

`validating` 先完整检查格式、条目、Manifest、资源限制和每个 Part 内容摘要，再检查目标
BlockIO Part 上限、bucket/ACL 和路径冲突。任何验证失败都发生在 BlockIO 上传和业务表
写入之前。dry-run 到此结束，只保留终态 Job，不创建 File、Part、Mapping 或后端对象。

普通恢复分为两层：

1. 为全部归档 File 分配新的 staged FileID；staged File 没有 Mapping。
2. layout v1 按归档 Part 边界逐个上传。每次上传必须得到 FileKey、DeleteRef 和
   UploadedAt，并在同一事务写 Part 与 `live` Delete State。
3. 每个新 Part 立即使用 FileKey 完整回读，校验 size 和 SHA-256 后才推进持久化游标。
4. 空 File 不调用 BlockIO；layout v2 只写 Segment 和 Completed Part，不重新上传内容。
5. 所有 File ready 后，`publishing` 在单一事务内再次检查冲突、创建父目录、create/replace
   Mapping、写 S3 Metadata 和 WebDAV Property，并生成当前数据库的新 change event。
6. replace 移除旧 File 的最后一个引用时，只把旧 `live` Delete State 改为 `pending`，
   物理删除仍由 durable worker 异步执行。

发布事务失败时没有部分新路径；staged File 仍不可见，补偿流程先建立 durable 删除状态，
再结束 Job。终态 Import 的接收 artifact 会被删除并清空 `artifact_path`。

正常 Part 与 `live` Delete State 在同一事务写入。如果该事务本身不能提交，恢复器会先用
独立事务保存补偿 Part 与 `pending` Delete State；只有连补偿记录也无法写入时，才对尚未
被数据库跟踪的上传结果执行同步删除。同步删除和补偿记录至少一个成功前，Job 不得进入
终态。

## 7. 重启、保留与空间

启动 worker 时会恢复所有非终态 Job：

- 未完成接收删除 partial 并进入 `canceled`；
- Export 清理旧 partial/snapshot/artifact、释放 Pin 后重新排队；
- validating Import 从完整 Verify 重新开始；
- 中断的 staging File 先通过 durable outbox 丢弃，再从归档重新建立；
- publishing Import 重新执行幂等发布事务；
- canceling Job 完成 Pin 或 staged File 补偿后进入终态。

周期清理负责：

- 删除到期 Export artifact，但保留 Job 并使 `artifact_available=false`；
- 删除终态 Import 的接收 artifact；
- 只在 artifact 已清理后删除超过 Job 保留期的 Job、stage 和 report；
- 清理由中断流程留下的孤立 Pin，其他 work dir 异常由 audit 报告。

接收 Import 前按 `Content-Length + max(1 GiB, 5%)` 检查空间；Export snapshot 后按
physical bytes 使用相同安全余量。长时间或大文件写入每 1 GiB 或 30 秒复查剩余空间。
work dir 固定为绝对路径、目录权限 `0700`、文件权限 `0600`；相对文件名在访问前再次
拒绝路径穿越。

## 8. HTTP 与权限

backup 功能默认关闭。开启后，所有路由先通过 `user_info` Basic Auth，再检查
`backup.users`：

- `read`：创建 Export、查询自己的 Job、下载自己的 artifact；
- `read-write`：具有 read 权限，并可 Import、查看全部 Job、取消 Job和读取全局指标。

| 方法 | 路由 | 语义 |
|---|---|---|
| `POST` | `/backup/v2/exports` | JSON `{"scope":"/"}`，创建 Export |
| `POST` | `/backup/v2/imports` | 接收 `.tgfb`，支持 `conflict=fail|replace` 与 `dry_run` |
| `GET` | `/backup/v2/jobs/{job_id}` | 查询 Job 状态、进度、结果和安全错误 |
| `POST` | `/backup/v2/jobs/{job_id}/cancel` | 请求取消未发布 Job |
| `GET/HEAD` | `/backup/v2/exports/{job_id}/artifact` | 下载完成的 Export artifact |
| `GET` | `/backup/v2/metrics` | read-write 用户读取 Prometheus 文本指标 |

artifact 支持 HEAD、Range 和 HTTP 条件请求，返回 Content-Length、媒体类型、下载文件名、
带引号的 artifact SHA-256 ETag、摘要 header 和 `Cache-Control: private, no-store`。
partial 文件永远不可下载。backup API 不接受普通 S3 Access Key 签名，也不接受 URL 拉取。

管理后台是同一 Job 引擎的独立入口。`admin.enable` 可以在不暴露 `/backup/v2` 的情况下
启动 backup manager；管理角色与 `backup.users` 互不继承。管理 Export 固定以登录用户名
作为 owner，管理 Job 列表使用 `(created_at, job_id)` keyset 分页。`read` 只能查看、下载
和取消自己的 Export；`read-write` 可以查看和取消所有可取消 Job，并执行 Import。

直接 `/backup/v2/imports` 继续要求客户端提交 artifact SHA-256。管理 Import 面向浏览器，
不要求浏览器预先计算摘要：manager 在接收 partial 文件时流式计算 SHA-256，完成 fsync 和
原子 rename 后把实际值持久化到 Job，再进入 queued。两种入口共享空间检查、接收、状态机、
归档校验、staging、发布和补偿逻辑。

## 9. 离线命令

三个 Cobra 子命令复用相同格式和 Job 引擎：

```bash
tgfile backup export --config=/config/config.json --scope=/ --output=/backup/full.tgfb
tgfile backup verify --config=/config/config.json --input=/backup/full.tgfb
tgfile backup import --config=/config/config.json --input=/backup/full.tgfb --conflict=fail
```

Export/Import 初始化数据库和配置的 BlockIO，并等待 Job 终态。Verify 只读取配置中的限制
和本地 artifact，不打开数据库、BlockIO、缓存或 HTTP。标准输入/输出不承载 artifact，
因为可靠导入导出需要多遍验证、fsync 和原子发布。

## 10. 审计与指标

只读 audit 报告 Job 状态、终态遗留 Pin、孤立 Pin、无 Pin 的活动 Export、活动 Job 缺失
work path、无效 stage target、意外可见的 staged File、Import Part 缺失 live Delete
State、过期 artifact、引用但缺失及孤立的 work file，以及 Telegram DeleteRef 的
bot/chat/message 身份异常。

Prometheus 文本指标为：

- `tgfile_backup_jobs_total{kind,result}`
- `tgfile_backup_active_jobs{kind,state}`
- `tgfile_backup_bytes_total{kind}`
- `tgfile_backup_duration_seconds{kind}`
- `tgfile_backup_failures_total{kind,code}`
- `tgfile_backup_artifact_bytes`
- `tgfile_backup_staged_files`

标签不包含 Job ID、路径、用户名、FileID 或其他高基数值。

## 11. 稳定不变量

- 外部路径和内容字节不随恢复改变；归档内 source FileID 只用于审计。
- 物理 Part 边界不能按目标 BlockIO 默认值重新切分，目标单 Part 上限必须足够大。
- staged File 在发布前没有 Mapping，任何协议入口都不能读取。
- 正常 Part 和 live Delete State 同事务持久化；已持久化 DeleteRef 的失败补偿只能进入
  durable outbox，未被持久化的上传必须先成功建立补偿记录或同步删除。
- Export Pin 参与最后引用判断，终态 Job 不得继续持有 Pin。
- S3 Metadata 和 WebDAV Property 按路径恢复到新 EntryID，不按新 FileID重新推导。
- 逻辑备份不迁移活动会话、锁、删除历史或凭据，也不承诺恢复原 Telegram message ID。
