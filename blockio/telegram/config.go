package telegram

type config struct { //tg bot 基础配置
	Chatid int64  `json:"chatid"`
	Token  string `json:"token"`
}
