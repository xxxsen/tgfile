package file

import (
	"encoding/hex"
	"fmt"
	"path"
	"unicode/utf8"

	"github.com/xxxsen/tgfile/utils"
)

const (
	defaultUploadPrefix           = "/defauls/"
	defaultMaxAllowFileNameLength = 128
)

func buildFileKeyLink(filename string, fileid uint64) (string, string) {
	fkey := hex.EncodeToString(utils.FileIdToHash(fileid))
	p1 := fkey[:2]
	base := path.Base(filename)
	ext := path.Ext(base)
	name := base[:len(base)-len(ext)]
	if len(name) == 0 {
		name = "noname"
	}
	if utf8.RuneCountInString(name) > defaultMaxAllowFileNameLength {
		name = string([]rune(name)[:defaultMaxAllowFileNameLength])
	}
	fkey += "-" + name + ext
	link := path.Join(defaultUploadPrefix, p1, fkey)
	return link, fkey
}

func extractLinkFromFileKey(fkey string) (string, error) {
	if len(fkey) <= 2 {
		return "", fmt.Errorf("invalid fkey:%s", fkey)
	}
	p1 := fkey[:2]
	return path.Join(defaultUploadPrefix, p1, fkey), nil
}
