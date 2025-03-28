tgfile
===

简易文件服务器, 将telegram当成无限存储使用。

基本原理: 文件上传的时候, 将文件切成多个块(单个块20M)并上传至telegram, 本地仅存储这块在telegram对应的文件id。

## 配置

一份简单的配置

```json
{
	"bind": ":9901", //监听地址
	"log_info": { //日志信息
		"console": true,
		"level": "debug"
	},
	"db_file": "/data/data.db", //索引存储位置
	"bot_info": { //用于存储文件块的机器人配置
		"chatid": 12345,
		"token": "abc"
	},
	"user_info": { //用户信息, 上传接口需要鉴权
		"abc": "123"
	},
	"s3_bucket": [ //启用s3协议支持, 这里配置的是要开启的s3 bucket名
		"hackmd"
	],
	"webdav": { //启用webdav支持, 实验性, 不一定ok
		"enable": true 
	}
}
```

## 运行

推荐使用docker运行

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

目前S3接口只实现了基本的GetObject/PutObject接口。

|API|Method|鉴权|备注|
|---|---|---|---|
|/:bucket|GET|false|获取bucket信息, 没实际作用|
|/:bucket/:object|PUT|true|文件上传|
|/:bucket/:object|GET|false|文件下载|

**Webdav接口**

默认根路径为 **/webdav**, 且不能修改。

可以通过下面命令验证:
```shell
curl -X PROPFIND -v https://your_username_here:your_pwd_here@your_host_here.com/webdav/ -L
```
