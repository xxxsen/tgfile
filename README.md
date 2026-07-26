# tgfile

tgfile 是一个以 Telegram 为主要内容后端的文件服务。文件按块上传到 Telegram，SQLite
保存文件、分片、路径、S3 元数据和可删除 message 引用；对外提供 S3、文件直链、WebDAV
和逻辑备份接口。

## 配置

下面是包含 Telegram、S3、WebDAV、逻辑备份与管理后台的完整配置示例：

```json
{
  "bind": ":9901",
  "log_info": {
    "console": true,
    "level": "info"
  },
  "db_file": "/data/data.db",
  "bot_kind": "telegram",
  "bot_config": {
    "chatid": 12345,
    "token": "telegram-bot-token",
    "upload_min_interval_ms": 1000
  },
  "user_info": {
    "access-key": "secret-key",
    "admin-viewer": "viewer-password",
    "admin-operator": "operator-password"
  },
  "s3": {
    "enable": true,
    "buckets": [
      {
        "name": "private-data",
        "acl": "private"
      },
      {
        "name": "public-assets",
        "acl": "public-read"
      }
    ],
    "max_object_size": 5368709120,
    "multipart_expire_hours": 24
  },
  "webdav": {
    "enable": true,
    "root": "/",
    "external_origin": "https://files.example.com",
    "max_upload_size": 5368709120,
    "upload_temp_dir": "/data/webdav-upload",
    "users": {
      "access-key": "read-write"
    },
    "quota_bytes": 0,
    "max_mutation_entries": 100000,
    "sync_page_size": 1000
  },
  "backup": {
    "enable": false,
    "work_dir": "/data/backup-work",
    "users": {
      "access-key": "read-write"
    },
    "max_archive_bytes": 107374182400,
    "max_expanded_bytes": 1099511627776,
    "max_mapping_count": 100000,
    "max_file_count": 100000,
    "max_part_count": 1000000,
    "max_path_bytes": 1024,
    "artifact_retention_hours": 24,
    "job_retention_days": 30
  },
  "admin": {
    "enable": true,
    "external_origin": [
      "https://files.example.com",
      "https://image.example.com"
    ],
    "users": {
      "admin-viewer": "read",
      "admin-operator": "read-write"
    },
    "session_idle_minutes": 30,
    "session_max_hours": 12,
    "max_upload_size": 5368709120
  },
  "io_cache": {
    "enable_l1_cache": true,
    "l1_cache_size": 16777216,
    "l1_key_size_limit": 4096,
    "enable_l2_cache": true,
    "l2_cache_size": 5368709120,
    "l2_key_size_limit": 524288,
    "l2_cache_dir": "/cache"
  }
}
```

bucket ACL：

- `private`：所有 S3 操作都需要认证；
- `public-read`：仅对象 GET/HEAD/GetObjectAttributes 可匿名，List、PUT、Copy 和 Delete
  仍需认证。

同一实例的 Telegram 上传请求串行执行，相邻上传的开始时间至少间隔配置值；删除请求也
串行执行，相邻删除的开始时间至少间隔一秒。因此 `bot_config.upload_min_interval_ms`
不能小于 `1000`。配置不会兼容旧的单一 `s3.bucket` 字段，bucket 必须显式写入
`s3.buckets` 并指定 ACL。

`s3.multipart_expire_hours` 控制未完成 Multipart Upload 的有效期。缺省或配置为 `0`
时使用 24 小时，显式值只能为 1～24；到期的暂存 part 会进入异步删除状态机。

WebDAV 使用 `user_info` 中的 Basic Auth 凭据，`webdav.users` 可把已知账号限制为
`read` 或 `read-write`；省略该 map 时所有已认证账号均为读写。部署在 HTTPS 反向代理后
应把 `external_origin` 配成客户端实际访问的 origin，用于严格校验 COPY/MOVE 的绝对
`Destination`，服务不会信任任意 `Forwarded` header。未知长度 PUT 会先流式写入
`upload_temp_dir`，完成计数后再进入 Telegram 分片上传；该目录必须位于持久化 volume 并
预留不小于 `max_upload_size` 的空间。`quota_bytes=0` 表示不限制逻辑配额，配额按
WebDAV root 内唯一 File 计费，COPY 同一内容不会重复计费。

逻辑备份默认关闭。开启时 `backup.users` 必须引用 `user_info` 中的账号：`read` 可以创建
导出、查询自己的任务和下载自己的归档；`read-write` 还可以导入、查看全部任务、取消任务
和读取备份指标。`work_dir` 必须是绝对路径并预留归档空间；服务创建该目录为 `0700`，
其中 artifact、snapshot 和报告为 `0600`。

Web 管理后台默认关闭。启用后访问 `https://files.example.com/_admin/`，账号密码来自
`user_info`，权限由独立的 `admin.users` 指定：`read` 可以浏览、下载、导出及管理自己的
导出任务，`read-write` 还可以上传、条件覆盖、导入及管理全部任务。建议使用不与 S3
Access Key 共用的专用高强度账号。

`admin.external_origin` 是数组，必须列出浏览器实际访问的 origin；登录和所有写请求的
`Origin`
必须精确命中其中一项。列表中的 origin 必须使用同一种 scheme，生产环境只接受 HTTPS，
本地 loopback 测试可以使用 HTTP。Session 只保存在进程内存，服务重启后需要重新登录。
`admin.enable` 与 `backup.enable` 相互独立；只启用管理后台时也会启动持久化导入导出
worker，但不会暴露 `/backup/v2` Basic Auth API。`backup.work_dir` 仍必须位于持久化
volume，并为导入归档和导出 artifact 预留足够空间。反向代理的请求体上限和读写超时必须
覆盖 `admin.max_upload_size` 与 `backup.max_archive_bytes`。

启动前可执行无副作用校验；该命令不会初始化日志、SQLite、Telegram、缓存或 HTTP：

```bash
./tgfile check-config --config=/config/config.json
```

## 运行

Docker Compose 示例：

```yaml
services:
  tgfile:
    image: xxxsen/tgfile:latest
    container_name: tgfile
    restart: always
    volumes:
      - ./config:/config:ro
      - ./data:/data
      - ./cache:/cache
    expose:
      - "9901"
    command: serve --config=/config/config.json
```

首次启动和升级启动都会根据嵌入式 `migrations/NNNN_name.sql` 更新 SQLite。无法识别的
旧 schema、migration checksum 变化或 schema 漂移会安全失败，不会重建业务数据。

## S3 使用

`user_info` 的 key/value 分别作为 access key 和 secret key：

```bash
export AWS_ACCESS_KEY_ID=access-key
export AWS_SECRET_ACCESS_KEY=secret-key
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url https://your-tgfile.example \
  s3 cp ./README.md s3://private-data/README.md

aws --endpoint-url https://your-tgfile.example \
  s3api head-object --bucket private-data --key README.md

aws --endpoint-url https://your-tgfile.example \
  s3api get-object \
  --bucket private-data \
  --key multipart.bin \
  --part-number 2 \
  multipart.part2.bin

aws --endpoint-url https://your-tgfile.example \
  s3api get-object-attributes \
  --bucket private-data \
  --key multipart.bin \
  --object-attributes ETag Checksum ObjectParts StorageClass ObjectSize \
  --max-parts 1000

aws --endpoint-url https://your-tgfile.example \
  s3api get-object \
  --bucket private-data \
  --key README.md \
  --response-content-disposition 'attachment; filename="README.md"' \
  downloaded-README.md

aws --endpoint-url https://your-tgfile.example \
  s3api copy-object \
  --bucket private-data \
  --copy-source private-data/README.md \
  --key archive/README.md

aws --endpoint-url https://your-tgfile.example \
  s3api delete-object --bucket private-data --key archive/README.md
```

使用 s3cmd 时必须配置 path-style endpoint。`host_bucket` 不得保留 s3cmd 默认的
`%(bucket)s.s3.amazonaws.com`，否则 bucket 请求会发往 AWS：

```ini
[default]
access_key = access-key
secret_key = secret-key
host_base = your-tgfile.example
host_bucket = your-tgfile.example
bucket_location = us-east-1
use_https = True
signature_v2 = False
check_ssl_certificate = True
check_ssl_hostname = True
acl_public = False
enable_multipart = True
multipart_chunk_size_mb = 15
```

`acl_public = False` 只阻止 s3cmd 探测或设置对象级 ACL，不改变 tgfile 配置的 bucket ACL。
Multipart 默认可用且没有服务端开关；`multipart_chunk_size_mb` 是客户端 S3 part 大小，
每个 S3 part 在 Telegram 后端仍会按 20 MiB 上限继续拆成物理 message。常用命令：

```bash
s3cmd ls s3://private-data/
s3cmd sync ./local-dir/ s3://private-data/backup/
s3cmd sync s3://private-data/backup/ ./restore-dir/
s3cmd del --recursive s3://private-data/backup/
```

不要使用 `s3cmd signurl`：s3cmd 2.4 生成 SigV2 URL，而 tgfile 只支持 SigV4 presigned
query。需要预签名 URL 时使用 AWS SDK、AWS CLI 或其他 SigV4 客户端。

支持的 S3 能力：

- ListBuckets、HeadBucket、GetBucketLocation；
- ListObjects V1/V2（prefix、delimiter、marker/token 分页、URL encoding）；
- PutObject 标准覆盖与条件写；
- GetObject、HeadObject、Range、HTTP 条件请求和六种 `response-*` 响应头覆盖；
- GetObject/HeadObject 的 `partNumber` 完成态 Part 读取；
- GetObjectAttributes 的 ETag、Checksum、ObjectParts、StorageClass、ObjectSize 和分页；
- CopyObject COPY/REPLACE；
- DeleteObject、DeleteObjects；
- CreateMultipartUpload、UploadPart、ListParts、CompleteMultipartUpload、
  AbortMultipartUpload、ListMultipartUploads；
- SigV4 header、presigned URL、signed/unsigned aws-chunked trailer；
- Content-MD5 以及 CRC32、CRC32C、CRC64NVME、SHA1、SHA256 checksum。

Multipart 最终对象支持 S3/直链/WebDAV 的完整读取、HEAD、Range、Copy 和 Delete。
Multipart additional checksum 支持 CRC32、CRC32C、CRC64NVME、SHA1 和 SHA256。
`partNumber` 使用 Complete 后连续的 final Part 编号，不是可能非连续的原 UploadPart 编号；
一个 S3 Part 仍可能跨多个 Telegram message。public-read bucket 的匿名请求如果携带任一
`response-*` 覆盖参数仍必须认证，因为这些参数属于 SigV4 canonical query。
UploadPartCopy、SSE、对象 ACL、bucket 创建/删除、版本控制、tagging 和 lifecycle
暂不支持。

显式 S3 删除/覆盖或 WebDAV DELETE/COPY/MOVE 覆盖移除内容的最后一个路径引用后，后台
worker 会在 Telegram 时限内尝试删除对应 message。WebDAV 成功响应表示路径变更和删除
任务已持久化，不表示 Telegram 已同步删除。逻辑删除成功仍保留 File、Part 和状态记录
用于审计。

## 其他接口

| 路由 | 方法 | 认证 | 说明 |
|---|---|---|---|
| `/file/upload` | POST | Basic | 直链上传并返回稳定 key |
| `/file/download/:key` | GET | 匿名 | 直链下载，支持 Range |
| `/file/meta/:key` | GET | 匿名 | 直链元数据 |
| `/file/purge` | POST | Basic | 清理没有删除状态的旧无引用元数据 |
| `/backup/v2/exports` | POST | Basic + backup role | 创建异步逻辑导出 |
| `/backup/v2/imports` | POST | Basic + read-write | 接收 `.tgfb` 并创建异步导入 |
| `/backup/v2/jobs/:job_id` | GET | Basic + backup role | 查询任务 |
| `/backup/v2/jobs/:job_id/cancel` | POST | Basic + read-write | 取消未发布任务 |
| `/backup/v2/exports/:job_id/artifact` | GET/HEAD | Basic + backup role | 下载完成归档 |
| `/backup/v2/metrics` | GET | Basic + read-write | Prometheus 文本指标 |
| `/static/*` | GET | Basic | 浏览目录树 |
| `/webdav/*` | WebDAV Class 1/2 + sync-collection | Basic | 映射 `webdav.root` |

## 逻辑备份

归档扩展名为 `.tgfb`，媒体类型为
`application/vnd.tgfile.backup.v2+tar+gzip`。HTTP Import 请求体直接发送归档，并要求
`Content-Length`、`Idempotency-Key` 和 `X-Tgfile-Artifact-SHA256`。例如：

```bash
curl -u access-key:secret-key \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: export-001' \
  -d '{"scope":"/"}' \
  https://your-tgfile.example/backup/v2/exports

curl -u access-key:secret-key \
  -H 'Content-Type: application/vnd.tgfile.backup.v2+tar+gzip' \
  -H 'Idempotency-Key: import-001' \
  -H "X-Tgfile-Artifact-SHA256: $(sha256sum full.tgfb | cut -d' ' -f1)" \
  --data-binary @full.tgfb \
  'https://your-tgfile.example/backup/v2/imports?conflict=fail&dry_run=false'
```

离线命令复用同一格式和持久化任务引擎：

```bash
./tgfile backup export \
  --config=/config/config.json \
  --scope=/ \
  --output=/backup/full.tgfb

./tgfile backup verify \
  --config=/config/config.json \
  --input=/backup/full.tgfb

./tgfile backup import \
  --config=/config/config.json \
  --input=/backup/full.tgfb \
  --conflict=fail
```

`verify` 不连接数据库或 Telegram。导入前建议先运行 `--dry-run`；`fail` 遇到现有文件即
失败，`replace` 原子覆盖同路径文件并让旧内容进入 durable 删除状态机。完整格式、恢复
事务、权限和限制见 [逻辑备份格式与恢复模型](docs/05-logical-backup.md)。

## 离线维护

只读审计不会执行 migration 或启动在线依赖：

```bash
./tgfile audit \
  --config=/config/config.json \
  --output=/maintenance/audit.json
```

检查直链 key：

```bash
./tgfile check-key --key='0123456789abcdef-example.txt'
```

## 本地开发

项目使用 Go 1.25.12：

```bash
make dev
make install-golangci-lint
make check
```

`make dev` 默认使用 `.dev-data/` 中的 localfile 和 SQLite，不连接 Telegram，并启用
S3、WebDAV 和 Web 管理后台。管理后台地址为 `http://localhost:9901/_admin/`，默认账号
密码为 `test / test`，仅限本地开发使用。开发参数可由 `TGFILE_DEV_HOST`、
`TGFILE_DEV_PORT`、`TGFILE_DEV_DATA_DIR`、`TGFILE_DEV_BUCKET`、`TGFILE_DEV_USERNAME`、
`TGFILE_DEV_PASSWORD` 和 `TGFILE_DEV_ADMIN_ORIGIN` 覆盖。

## 设计文档

- [系统架构](docs/01-architecture.md)
- [数据与存储模型](docs/02-data-and-storage-model.md)
- [核心流程与接口设计](docs/03-core-flows-and-api.md)
- [WebDAV 协议与资源模型](docs/04-webdav-protocol.md)
- [逻辑备份格式与恢复模型](docs/05-logical-backup.md)
- [Web 管理后台](docs/06-web-management.md)
