package file

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"

	"github.com/xxxsen/tgfile/utils"
)

const (
	defaultUploadPrefix           = "/defaults/"
	defaultMaxAllowFileNameLength = 128
	defaultMaxAllowExtLength      = 16
	fileKeyHashLength             = 16
	maxFileKeyLength              = fileKeyHashLength + 1 + defaultMaxAllowFileNameLength
)

var (
	errInvalidFileKeyLength    = errors.New("invalid file key length")
	errInvalidFileKeySeparator = errors.New("invalid file key separator")
	errInvalidFileKeyHash      = errors.New("invalid file key hash")
	errInvalidFileKeySuffix    = errors.New("invalid file key suffix")
	defaultFileNameCleaner     = regexp.MustCompile(`[\\/:*?"<>|+#%{}'&$@!~\(\)\[\]^` + "`" + ` ]`)
)

func (h *FileHandler) removeInvalidChar(name string) string {
	return defaultFileNameCleaner.ReplaceAllString(name, "")
}

func (h *FileHandler) tryCutBaseName(base string) string {
	// 尽可能地保持extname
	if len(base) <= defaultMaxAllowFileNameLength {
		return base
	}
	ext := path.Ext(base)
	name := base[:len(base)-len(ext)]
	if len(ext) > defaultMaxAllowExtLength { // 异常的extname, 那么直接将base截断即可
		return base[:defaultMaxAllowFileNameLength]
	}
	name = name[:defaultMaxAllowFileNameLength-len(ext)]
	return name + ext
}

func (h *FileHandler) buildFileKeyLink(filename string, fileid uint64) (string, string) {
	fkey := hex.EncodeToString(utils.FileIdToHash(fileid))
	p1 := fkey[:2]
	base := path.Base(filename)
	base = h.tryCutBaseName(h.removeInvalidChar(base))
	fkey = fkey + "-" + base
	link := path.Join(defaultUploadPrefix, p1, fkey)
	return link, fkey
}

func (h *FileHandler) extractLinkFromFileKey(fkey string) (string, error) {
	return ExtractLinkFromFileKey(fkey)
}

func ExtractLinkFromFileKey(fkey string) (string, error) {
	if len(fkey) < fileKeyHashLength+1 || len(fkey) > maxFileKeyLength {
		return "", fmt.Errorf("%w: %d", errInvalidFileKeyLength, len(fkey))
	}
	if fkey[fileKeyHashLength] != '-' {
		return "", errInvalidFileKeySeparator
	}
	for i := 0; i < fileKeyHashLength; i++ {
		value := fkey[i]
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return "", fmt.Errorf("%w at offset %d", errInvalidFileKeyHash, i)
		}
	}
	for i := fileKeyHashLength + 1; i < len(fkey); i++ {
		value := fkey[i]
		if value == '/' || value == '\\' || value == 0 || value < 0x20 || value == 0x7f {
			return "", fmt.Errorf("%w at offset %d", errInvalidFileKeySuffix, i)
		}
	}
	return defaultUploadPrefix + fkey[:2] + "/" + fkey, nil
}
