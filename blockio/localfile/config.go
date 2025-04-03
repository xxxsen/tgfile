package localfile

type config struct {
	Dir       string `json:"dir"`
	BlockSize int64  `json:"block_size"`
}
