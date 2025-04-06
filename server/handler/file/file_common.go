package file

import (
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/xxxsen/tgfile/utils"
)

const (
	defaultUploadPrefix           = "/defauls/"
	defaultMaxAllowFileNameLength = 128
	defaultMaxAllowExtLength      = 16
)

var (
	defaultFileNameCleaner = regexp.MustCompile(`[\\/:*?"<>|+#%{}'&$@!~\(\)\[\]^` + "`" + ` ]`)
)

func removeInvalidChar(name string) string {
	return defaultFileNameCleaner.ReplaceAllString(name, "")
}

func tryCutBaseName(base string) string {
	//尽可能地保持extname
	if len(base) <= defaultMaxAllowFileNameLength {
		return base
	}
	ext := path.Ext(base)
	name := base[:len(base)-len(ext)]
	if len(ext) > defaultMaxAllowExtLength { //异常的extname, 那么直接将base截断即可
		return base[:defaultMaxAllowFileNameLength]
	}
	name = name[:defaultMaxAllowFileNameLength-len(ext)]
	return name + ext
}

func buildFileKeyLink(filename string, fileid uint64) (string, string) {
	fkey := hex.EncodeToString(utils.FileIdToHash(fileid))
	p1 := fkey[:2]
	base := path.Base(filename)
	base = tryCutBaseName(removeInvalidChar(base))
	fkey = fkey + "-" + base
	link := path.Join(defaultUploadPrefix, p1, fkey)
	return link, fkey
}

func extractLinkFromFileKey(fkey string) (string, error) {
	fkey = removeInvalidChar(fkey)
	idx := strings.Index(fkey, "-")
	if idx < 0 {
		return "", fmt.Errorf("no seperator found")
	}
	prefix := fkey[:idx]
	if len(fkey) <= 2 {
		return "", fmt.Errorf("invalid fkey:%s", fkey)
	}
	p1 := prefix[:2]
	for _, c := range p1 {
		if !(unicode.IsDigit(c) || unicode.IsLetter(c)) {
			return "", fmt.Errorf("invalid char in suffix, c:%c", c)
		}
	}
	return path.Join(defaultUploadPrefix, p1, fkey), nil
}
