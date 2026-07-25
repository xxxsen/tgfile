# tgfile

tgfile 是一个以 Telegram 为主要内容后端的文件服务。文件按块上传到 Telegram，SQLite
保存文件、分片、路径、S3 元数据和可删除 message 引用；对外提供 S3、文件直链、WebDAV
和逻辑备份接口。

## 配置

下面是 Telegram + S3 的最小完整示例：

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
    "access-key": "secret-key"
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
    "max_object_size": 5368709120
  },
  "webdav": {
    "enable": false,
    "root": "/"
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
- `public-read`：仅对象 GET/HEAD 可匿名，List、PUT、Copy 和 Delete 仍需认证。

同一实例的 Telegram 上传请求串行执行，相邻上传的开始时间至少间隔配置值；删除请求也
串行执行，相邻删除的开始时间至少间隔一秒。因此 `bot_config.upload_min_interval_ms`
不能小于 `1000`。配置不会兼容旧的单一 `s3.bucket` 字段，bucket 必须显式写入
`s3.buckets` 并指定 ACL。

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
enable_multipart = False
```

`acl_public = False` 只阻止 s3cmd 探测或设置对象级 ACL，不改变 tgfile 配置的 bucket ACL。
`enable_multipart = False` 关闭的是客户端 S3 Multipart Upload；tgfile 仍会按 BlockIO 限制
对普通 PutObject 内部分片。常用命令：

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
- GetObject、HeadObject、Range 和 HTTP 条件请求；
- CopyObject COPY/REPLACE；
- DeleteObject、DeleteObjects；
- SigV4 header、presigned URL、signed/unsigned aws-chunked trailer；
- Content-MD5 以及 CRC32、CRC32C、CRC64NVME、SHA1、SHA256 checksum。

不支持 bucket 创建/删除、对象 ACL、版本控制、multipart upload、tagging 和 lifecycle。

显式 S3 删除或覆盖移除内容的最后一个路径引用后，后台 worker 会在 Telegram 时限内尝试
删除对应 message。逻辑删除成功仍保留 File、Part 和状态记录用于审计。

## 其他接口

| 路由 | 方法 | 认证 | 说明 |
|---|---|---|---|
| `/file/upload` | POST | Basic | 直链上传并返回稳定 key |
| `/file/download/:key` | GET | 匿名 | 直链下载，支持 Range |
| `/file/meta/:key` | GET | 匿名 | 直链元数据 |
| `/file/purge` | POST | Basic | 清理没有删除状态的旧无引用元数据 |
| `/backup/export` | GET | Basic | 导出逻辑文件树 tar.gz |
| `/backup/import` | POST | Basic | 导入逻辑文件树 |
| `/static/*` | GET | Basic | 浏览目录树 |
| `/webdav/*` | WebDAV | Basic | 映射 `webdav.root` |

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

`make dev` 默认使用 `.dev-data/` 中的 localfile 和 SQLite，不连接 Telegram。开发参数可由
`TGFILE_DEV_HOST`、`TGFILE_DEV_PORT`、`TGFILE_DEV_DATA_DIR`、`TGFILE_DEV_BUCKET`、
`TGFILE_DEV_USERNAME` 和 `TGFILE_DEV_PASSWORD` 覆盖。

## 设计文档

- [系统架构](docs/01-architecture.md)
- [数据与存储模型](docs/02-data-and-storage-model.md)
- [核心流程与接口设计](docs/03-core-flows-and-api.md)
