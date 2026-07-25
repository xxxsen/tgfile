package s3

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/entity"
)

const (
	objectReadModeFull       = "full"
	objectReadModeRange      = "range"
	objectReadModePart       = "part"
	objectReadModeAttributes = "attributes"
)

type objectReadObservation struct {
	operation             string
	readMode              string
	attributes            string
	manifestChecksumState string
	partPageSize          int
	partPageTruncated     bool
}

func newObjectReadObservation(operation, mode string) *objectReadObservation {
	return &objectReadObservation{operation: operation, readMode: mode}
}

func (observation *objectReadObservation) finish(c *gin.Context) {
	resultCode := "OK"
	if value, exists := c.Get("s3-result-code"); exists {
		if code, valid := value.(string); valid && code != "" {
			resultCode = code
		}
	} else if c.Writer.Status() >= http.StatusMultipleChoices {
		resultCode = http.StatusText(c.Writer.Status())
	}
	logutil.GetLogger(c.Request.Context()).Info(
		"S3 object read completed",
		zap.String("s3_operation", observation.operation),
		zap.String("read_mode", observation.readMode),
		zap.String("result_code", resultCode),
		zap.Int("status_code", c.Writer.Status()),
		zap.String("attributes", observation.attributes),
		zap.String("manifest_checksum_state", observation.manifestChecksumState),
		zap.Int("part_page_size", observation.partPageSize),
		zap.Bool("part_page_truncated", observation.partPageTruncated),
	)
}

func (observation *objectReadObservation) setAttributes(attributes objectAttributeSet) {
	names := make([]string, 0, 5)
	if attributes.etag {
		names = append(names, "ETag")
	}
	if attributes.checksum {
		names = append(names, "Checksum")
	}
	if attributes.objectParts {
		names = append(names, "ObjectParts")
	}
	if attributes.storageClass {
		names = append(names, "StorageClass")
	}
	if attributes.objectSize {
		names = append(names, "ObjectSize")
	}
	sort.Strings(names)
	observation.attributes = strings.Join(names, ",")
}

func completedPartChecksumState(parts []entity.S3CompletedPart) string {
	if len(parts) == 0 {
		return ""
	}
	state := parts[0].ChecksumState
	for _, part := range parts[1:] {
		if part.ChecksumState != state {
			return "mixed"
		}
	}
	return state
}
