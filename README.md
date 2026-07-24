tgfile
===

简易文件服务器, 将telegram当成无限存储使用。

基本原理: 文件上传的时候, 将文件切成多个块(单个块20M)并上传至telegram, 本地仅存储这块在telegram对应的文件id。

## 配置

### 服务端

**基础配置:**

```jsonc
{
	"bind": ":9901", //监听地址
	"log_info": { //日志信息
		"console": true,
		"level": "debug"
	},
	"db_file": "/data/data.db", //索引存储位置
	"bot_info": { //用于存储文件块的机器人配置
		"chatid": 12345, //用户自己的chatid, 通过其他机器人获取到自己的chatid, 然后自己再主动跟机器人发起会话, 后面上传的文件会发给这个chatid进行存储 
		"token": "abc"
	},
	"user_info": { //用户信息, 上传接口需要鉴权
		"abc": "123"
	},
	"s3": {
		"enable": true, //启用s3协议支持, 这里配置的是要开启的s3 bucket名
		"bucket":[ 
			"hackmd"
		]
	},
	"webdav": { //启用webdav支持
		"enable": true,
		"root": "/"    //指定映射到底层存储的路径, 与接口上的'/webdav'不是一个东西
	}
	//其他配置项
}
```

**io缓存:**

由于底层对接的是网络io(目前为telegram), 速率相对较慢, 一个小文件, 获取链接+下载完成大概需要1~2s的时间, 为了加快小文件下载过程, 可以考虑加上缓存配置

```jsonc
{ 
    "io_cache": { //与`bind`同级
        "enable_l1_cache": true, //启用l1缓存
        "l1_cache_size": 16777216, //l1缓存占用(内存), 16M
        "l1_key_size_limit": 4096, //4K, 文件大小小于该值才可以被加入l1缓存
        "enable_l2_cache": true, //启用l2缓存
        "l2_cache_size": 5368709120, //l2缓存占用(磁盘), 5G
        "l2_key_size_limit": 524288, //512K, 文件大小小于该值才可以被加入l2缓存
        "l2_cache_dir": "/tmp/tgfile-cache" //l2缓存的数据存储目录
    }
}
```

### 客户端

非必要配置, 如果想在本地通过命令行上传文件才需要客户端配置, 用户也可以通过其他方式进行文件上传, 如s3, webdav.

```jsonc
{
    "schema": "http",
    "host": "abc.example.com:9901", 
    "access_key": "aaa", //用户名
    "secret_key": "1111", //密码,
    "thread": 5,  //指定上传时分块上传的线程数
    "timeout": 600 //连接超时时间
}
```

客户端搜索配置会在下面几个路径下搜索, 优先级由高到低

- 用户自己指定的配置, 通过--config传入
- /etc/tgc/tgc_config.json (windows则为c:/tgc/tgc_config.json)
- 基于环境变量 TGC_CONFIG 指定

## 运行

**服务端**使用docker运行

```
services:
  tgfile:
    image: xxxsen/tgfile:latest
    container_name: "tgfile"
    restart: always
    volumes:
      - "./config:/config"
      - "./data:/data"
    expose:
      - 9901
    command: -config=/config/config.json
```

- config目录: 存储配置文件
- data目录: 存储索引信息

对于**客户端**, 直接二进制运行, 可以通过release下载二进制文件, 或者通过`go install github.com/xxxsen/tgfile/cmd/tgc@latest` 安装最新的版本。

在`/etc/tgc`(如果是windows则路径为:`C:/tgc`)下创建tgc_config.json 配置, 之后执行下面命令即可进行文件上传。

```shell
# 如果下载回来的文件名不为tgc, 建议重命名为tgc, 通过go install安装则名字为`tgc`
tgc upload --file=./README.md
```

上传完成后, 会返回一个链接, 通过链接即可下载刚刚上传的文件。

### 本地开发

安装 Go 1.25 后，可通过下面的命令快速启动本地测试服务器：

```shell
make dev
```

默认服务地址为 `http://127.0.0.1:9901`，S3 bucket 为 `hackmd`，开发账号为
`dev / dev-secret`。开发服务使用 `.dev-data/` 下独立的 SQLite 数据库和本地文件块，
不会连接 Telegram，也不会读取或修改正式配置。按 `Ctrl+C` 会停止服务，开发数据会保留。

可以通过 `TGFILE_DEV_HOST`、`TGFILE_DEV_PORT`、`TGFILE_DEV_DATA_DIR`、`TGFILE_DEV_BUCKET`、
`TGFILE_DEV_USERNAME` 和 `TGFILE_DEV_PASSWORD` 覆盖默认值。也可以执行
`make dev CONFIG=path/to/config.json` 使用自定义配置；此时若配置使用了其他端口，需要同时
设置 `TGFILE_DEV_PORT`，以便启动脚本进行就绪检测。

## 接口信息

**基础接口**

|API|Method|鉴权|备注|
|---|---|---|---|
|/file/upload|POST|true|文件上传|
|/file/download/:key|GET|false|下载文件, key通过/file/upload获取|
|/file/meta/:key|GET|false|获取文件信息, key通过/file/upload获取|

**备份接口**

|API|Method|鉴权|备注|
|---|---|---|---|
|/backup/export|GET|true|将当前存储的所有文件打包成tar.gz并导出|
|/backup/import|POST|true|将export导出的tar.gz文件导入到新的实例中|

**文件枚举**

|API|Method|鉴权|备注|
|---|---|---|---|
|/static|GET|true|展示目录文件列表, 类似`python3 -m http.server 8000`|

**S3接口**

目前S3接口实现了基本的GetObject/HeadObject/PutObject接口。

|API|Method|鉴权|备注|
|---|---|---|---|
|/:bucket|GET|false|获取bucket信息, 没实际作用|
|/:bucket/:object|PUT|true|文件上传|
|/:bucket/:object|GET|false|文件下载|
|/:bucket/:object|HEAD|false|获取文件元数据，不读取文件内容|

**Webdav接口**

|API|Method|鉴权|备注|
|---|---|---|---|
|/webdav|GET/...|true|起始路径为'/webdav', 具体底层映射到哪个路径, 由配置的root决定|

可以通过下面命令验证:
```shell
curl -X PROPFIND -v https://your_username_here:your_pwd_here@your_host_here.com/webdav/ -L
```
