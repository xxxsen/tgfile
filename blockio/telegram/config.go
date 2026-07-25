package telegram

type config struct { // tg bot 基础配置
	Chatid              int64  `json:"chatid"`
	Token               string `json:"token"`
	UploadMinIntervalMS int64  `json:"upload_min_interval_ms"`
}
