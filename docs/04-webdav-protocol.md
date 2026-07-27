# WebDAV 协议与资源模型

本文定义 tgfile WebDAV 的稳定协议语义、事务边界和 Telegram 约束。系统组件见
[`01-architecture.md`](01-architecture.md)，协议元数据表见
[`02-data-and-storage-model.md`](02-data-and-storage-model.md)，其他 HTTP/S3 流程见
[`03-core-flows-and-api.md`](03-core-flows-and-api.md)。

## 1. 能力边界

WebDAV 入口为 `/webdav/*`，使用 Basic Auth，通过 `webdav.root` 映射到与 S3、直链和
管理后台共用的 Mapping 树。服务声明：

```text
DAV: 1, 2, sync-collection
```

支持的方法为 OPTIONS、GET、HEAD、PUT、DELETE、MKCOL、COPY、MOVE、PROPFIND、
PROPPATCH、LOCK、UNLOCK 和 REPORT。Class 2 第一版只支持 exclusive write lock；
collection sync 只实现 RFC 6578 的 `sync-collection`。不支持随机写、PATCH、Extended
MKCOL、逐资源 ACL、版本控制和 WebDAV ACL。

Telegram 只提供不可变 message 内容存储和删除能力，不提供目录、属性、锁、配额或同步
版本。这些语义全部由 SQLite 和 FileManager 实现。任何 WebDAV 成功响应都不等待 Telegram
网络删除。

## 2. 认证、权限和缓存

账号必须同时存在于全局 `user_info` 和 `user_permission`。权限派生为：

- `webdav:read`：只允许 OPTIONS、GET、HEAD、PROPFIND 和 REPORT；
- `webdav:write`：允许全部已实现方法，并自动包含读能力。

没有 WebDAV 权限的已认证账号返回 403；不存在“所有已认证账号默认读写”的回退。只读账号
对写方法返回 403，并在 `Allow` 中只公布只读方法。未知长度 PUT 在创建 spool 文件前完成
`webdav:write` 判断。服务不实现逐目录或逐资源 ACL，S3 bucket 的 `private/public-read`
也不会转换为 WebDAV 权限。

所有 WebDAV 响应使用：

```text
Cache-Control: private, no-cache
Vary: Authorization
```

鉴权内容不得复用匿名直链或 public S3 的共享缓存策略。生产入口必须使用 HTTPS；Basic
Auth 凭据本身不提供传输加密。

## 3. 路径和 Destination

请求路径按 URL path segment 解析，拒绝 NUL、反斜线以及编码后的 `/`、`\`，规范化后必须
仍在 `webdav.root` 内。响应 href 对每个路径的 UTF-8 字节进行 URI 转义，collection 保留
尾部 `/`。

COPY/MOVE 的 Destination 可使用 origin-form 或 absolute-form。它必须位于恰好
`/webdav` 或 `/webdav/` 子树，不能依赖字符串前缀接受 `/webdav2`。absolute-form 的
scheme、host 和 port 必须与服务认定的 external origin 一致：

- 配置了顶层 `external_origin` 数组时，匹配其中任一规范化后的 HTTP(S) origin；
- 未配置时，以直连请求的 TLS 状态和 Host 为准；
- 不信任任意 `Forwarded` 或 `X-Forwarded-*` header。

该顶层数组同时供管理后台执行 Origin 校验，不在 `webdav` 和 `admin` 子项重复配置。

跨 origin Destination 返回 502，非法 URI 或越界路径返回 400，源和目标是同一资源返回
403。

## 4. 读取和条件请求

文件 GET、HEAD 和 PROPFIND live properties 使用同一份 Mapping 数据：

- 强 ETag 为 `"<file-id>-<file-size>"`；
- Last-Modified 使用 Mapping mtime 的 UTC 秒精度；
- Content-Type 按文件名推断；
- Content-Length 使用完整 File 大小；
- GET 支持单 Range、多 Range和 If-Range，HEAD 只返回对应表示的 headers。

支持 If-Match、If-None-Match、If-Modified-Since 和 If-Unmodified-Since。条件失败不携带
representation Content-Length；304 和 412 不打开 Telegram stream。layout v1 和 S3
Multipart 完成后的 layout v2 都通过 `FileManager.OpenFile` 读取，因此 Range 可以跨
Telegram message 或 Composite segment 边界。

collection 没有文件字节表示，GET/HEAD 返回 405；目录元数据通过 PROPFIND 读取。

## 5. 写入与资源方法

### 5.1 PUT

PUT 必须有已知最终长度。HTTP request 使用 chunked encoding 时，server 在鉴权后把请求体
流式写入受限临时文件；超过 `max_upload_size` 返回 413，完成后用计数结果设置长度并进入
原有 File 分片上传。临时文件在成功、失败和连接关闭时删除，启动时清理达到保留阈值的
遗留 spool。

底层按 BlockIO 单块上限创建 layout v1 File；Telegram 后端每 20 MiB 创建一个 message。
上传完成后，`PublishWebDAVFile` 在一个 SQLite 事务中：

1. 重读目标 Mapping 并校验 HTTP/WebDAV 条件和锁；
2. 校验逻辑 quota 和新 File 是否可引用；
3. 新路径创建 Mapping，已有文件原地替换 `ref_data/file_size/mtime`；
4. 保留覆盖资源的 `entry_id` 和 dead properties；
5. 更新跨协议 S3 Metadata；
6. lock-null 资源转为普通资源；
7. 旧 File 最后引用消失时把 durable delete state 置为 `pending`。

目标不存在返回 201，覆盖文件返回 204，覆盖 collection 返回 405，父 collection 不存在
返回 409。事务发布失败时丢弃尚未发布的新 File，旧 Mapping 始终保持可读。

### 5.2 MKCOL、COPY、MOVE 和 DELETE

- MKCOL 不隐式创建祖先；父 collection 缺失返回 409，目标已存在返回 405，不支持请求体
  返回 415。
- COPY collection 接受 Depth 0 或 infinity，缺省为 infinity。Depth 0 只复制空
  collection；文件可接受缺省、0 或 infinity。
- MOVE collection 和 DELETE collection 只接受缺省或 infinity。
- Overwrite 只接受 `T`、`F` 或缺省；缺省等同 `T`。目标存在且为 `F` 返回 412。
- COPY/MOVE 根据操作前目标是否存在返回 201 或 204；DELETE 成功返回 204，不存在返回
  404。
- 递归 DELETE/COPY/MOVE 在执行前统计源子树，超过 `max_mutation_entries` 返回 507，
  不开始部分修改。

COPY 复用不可变 File，不复制 Telegram 字节；MOVE 保持 FileID 和 entry ID。覆盖和删除
递归处理 Mapping、S3 Metadata、dead properties、locks、change journal 和最终 File 引用，
并与 durable outbox 状态变化处于同一 SQLite 事务。

## 6. PROPFIND 和 PROPPATCH

PROPFIND 支持 `prop`、`allprop`、`allprop + include` 和 `propname`，请求 XML 有固定大小
上限。Depth 0 返回目标，Depth 1 流式返回目标和按 entry ID 稳定分页的直接子项。
Depth 缺省按 infinity 解释；服务不执行无限递归，而返回包含
`DAV:propfind-finite-depth` 的 403。非法 Depth 返回 400。

live properties 包括：

- displayname、creationdate、getlastmodified；
- getcontentlength、getcontenttype、getetag；
- resourcetype、supportedlock、lockdiscovery；
- supported-report-set、sync-token；
- 配置逻辑 quota 时的 quota-used-bytes、quota-available-bytes。

已知且适用于资源的属性位于 200 propstat，不存在或不适用的属性位于 404 propstat。时间
统一输出 UTC；collection 使用空 `DAV:collection` 元素。

PROPPATCH 按文档顺序解析 set/remove，在一次 SQLite 事务中全部成功或全部回滚。`DAV:`
live properties 是受保护属性：对应 property 返回 403，同请求其他 property 返回 424，
不会写入部分 dead property。成功响应为 207，各属性返回 200。

dead property 生命周期：

- PUT 原地覆盖保留；
- COPY 复制，MOVE 保留；
- DELETE 和覆盖删除清理；
- S3 同路径覆盖重新绑定到新 Mapping；
- S3 CopyObject 复制源属性，覆盖时先清理目标属性。

## 7. LOCK、UNLOCK 和 WebDAV If

LOCK 只接受 exclusive write、Depth 0/infinity。token 是不可预测的
`opaquelocktoken:<uuid>`，owner XML 和超时时间持久化在 SQLite；客户端请求的 timeout
最大限制为 24 小时。depth infinity 锁保护根及全部后代。

对不存在 URL 执行 LOCK 时，父 collection 必须存在。服务创建零字节 lock-null 文件并返回
201；首次 PUT 保留 entry ID 并清除 lock-null 标记。若未写入就 UNLOCK，或锁过期后触发
清理，lock-null Mapping 及其未引用 File 会按正常事务和删除状态机移除。

refresh LOCK 使用空 body 和包含唯一正 lock token 的 If header。UNLOCK 要求规范的
Lock-Token header、相同资源和相同 principal。锁和 refresh 在服务重启后仍有效。

PUT、DELETE、MKCOL、PROPPATCH、COPY destination、MOVE source/destination 都在最终
SQLite 事务中检查锁。WebDAV If 支持 tagged/untagged condition list、Not、lock token 和
entity-tag；任一 OR list 满足其全部 AND terms 即通过。缺失或错误 token 返回 423，普通
HTTP 条件或 If list 不满足返回 412。

## 8. 逻辑 Quota

Quota 是配置级逻辑上限，不表示 Telegram 物理剩余空间。used bytes 在 WebDAV root 子树中
按唯一 live FileID 计费：

- 多个 Mapping 或 COPY 引用同一 File 只计一次；
- 覆盖时扣除失去最后一个 root 内引用的旧 File，再计入尚未引用的新 File；
- 超限 PUT 返回 507，条件判断和配额判断都在发布事务中执行；
- 配额只约束 WebDAV 写入；同一 Mapping 树上的 S3 写入不受该 WebDAV 配置阻止，但会计入
  后续 quota-used-bytes；
- 未配置 quota 时不暴露 quota properties。

## 9. Collection Sync

PROPFIND 的 supported-report-set 和 sync-token 暴露 `sync-collection`。REPORT 要求
Depth 0，请求中的 sync-level 支持 1 和 infinite；输出使用 207：

- 空 sync-token：按 sync-level 流式返回当前直接子项或完整后代，并签发当前 journal
  revision；
- 非空 token：返回该 revision 之后作用域内每个路径的最新变更；
- 删除使用 404 response tombstone；
- 增量变更每页最多 `sync_page_size` 条，页尾 token 是已返回的最后 revision；初始快照按
  当前作用域流式输出，不在内存中构造完整列表；
- 未来 revision、格式错误或不属于当前 journal 的 token 返回包含
  `DAV:valid-sync-token` 的 403。

Directory 的 S3 和 WebDAV mutation 共用 change journal，所以从任一协议创建、覆盖、复制、
移动或删除 Mapping 都能被 WebDAV 同步客户端观察到。journal 不读取 Telegram 内容。

## 10. 数据与并发不变量

- handler 只解析协议，不直接修改业务表；FileManager 拥有最终条件、锁、配额和生命周期
  语义。
- Mapping 变更、S3 Metadata、dead properties、locks、journal、最终引用判断和
  `live -> pending` 状态变化必须处于同一 SQLite 事务。
- Telegram 上传先于 Mapping 发布；发布失败必须补偿未发布 File。Telegram 删除只能由
  durable outbox worker 异步执行。
- COPY 只增加 File 引用，MOVE 不改变 FileID；任何仍被 S3/WebDAV/Composite/Multipart
  引用的 File 都不能进入删除队列。
- 强 ETag 由不可变 FileID 和大小构造，不为此下载或哈希 Telegram 内容。
- 新增协议表不回填或改写历史 File、Part、Mapping、FileKey、DeleteRef 和对象字节；
  历史资源天然表示没有 dead properties、锁和待同步的旧变更。
