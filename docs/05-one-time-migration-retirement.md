# 一次性路径迁移逻辑退休记录

记录日期：2026-07-25  
适用范围：v0.0.33 上线后的后续版本

## 1. 结论

首次生产上线已经完成默认上传根目录迁移，并验证数据库、存量 S3 读取和新上传链路。
后续版本不再承担旧目录数据库的在线或离线兼容，因此删除一次性迁移工具和反向回滚
分支。运行时唯一合法的默认上传根目录为 `/defaults`。

本次退休只删除代码能力，不再次修改生产数据库、SQLite schema、文件记录、分片记录
或后端块。

## 2. 已退休内容

- `migrate-default-prefix` Cobra 子命令；
- forward、reverse 和 dry-run 路径迁移实现；
- already-migrated 容错分支及迁移前置条件类型；
- audit 中区分新旧目录的两个字段；
- 旧目录迁移单元测试和集成测试；
- lint 对历史拼写的临时忽略规则。

audit 继续检查当前目录不变量，字段统一为：

```json
{
  "default_root_exists": true
}
```

## 3. 保留的兼容边界

以下内容与存量数据读取直接相关，不能因本次清理而改变：

- file_id、Telegram FileKey、分片顺序和 `ref_data`；
- S3 bucket/object 路径；
- 外部文件直链 key；
- 存量 MD5 的计算和返回格式；
- `rotate_stream` 编解码语义；
- S3 GET、HEAD、Range 和已存在对象返回 409 的行为。

## 4. 生产依据

退休前已经确认：

- SQLite `quick_check` 和 `integrity_check` 均为 `ok`；
- 根目录迁移只修改一条 mapping 的 `file_name`，其他字段和行数不变；
- 迁移后 audit 不存在缺失 mapping、非 Ready mapping 或分片数异常；
- 存量 S3 样本的 GET、HEAD、Range、长度和 MD5 一致；
- SigV4 PUT、GET、HEAD 成功，重复 PUT 返回 409 且内容不变；
- 迁移前数据库、配置和部署文件已有受限权限备份及 SHA-256 清单。

## 5. 后续发布和恢复

部署包含本次退休改动的镜像不需要再次停服迁移数据，只需确认目标数据库已经存在
`/defaults` 根目录，并执行只读 audit。

当前代码不再提供恢复旧目录的命令。灾难恢复必须使用首次上线时保存的不可变镜像和
已验证备份，并在停服状态下整体恢复。不得把当前镜像连接到未迁移数据库，也不得为
兼容旧备份重新引入运行时双读或 fallback。
