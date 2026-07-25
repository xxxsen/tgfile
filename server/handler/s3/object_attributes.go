package s3

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
)

const (
	defaultObjectAttributesMaxParts = 1000
	objectStorageClassStandard      = "STANDARD"
)

var (
	errObjectChecksumValueMissing      = errors.New("object checksum value is missing")
	errObjectSHA256Inconsistent        = errors.New("object SHA256 checksums are inconsistent")
	errObjectChecksumUnsupported       = errors.New("object checksum algorithm is unsupported")
	errPartChecksumWithoutObjectPolicy = errors.New("part checksum exists without an object checksum algorithm")
	errPartChecksumAlgorithmMismatch   = errors.New("part checksum algorithm differs from object metadata")
)

type objectAttributeSet struct {
	etag         bool
	checksum     bool
	objectParts  bool
	storageClass bool
	objectSize   bool
}

type getObjectAttributesResponse struct {
	XMLName      xml.Name                  `xml:"GetObjectAttributesResponse"`
	XMLNS        string                    `xml:"xmlns,attr"`
	ETag         *string                   `xml:"ETag,omitempty"`
	Checksum     *objectAttributesChecksum `xml:"Checksum,omitempty"`
	ObjectParts  *objectAttributesParts    `xml:"ObjectParts,omitempty"`
	StorageClass string                    `xml:"StorageClass,omitempty"`
	ObjectSize   *int64                    `xml:"ObjectSize,omitempty"`
}

type objectAttributesChecksum struct {
	CRC32     string `xml:"ChecksumCRC32,omitempty"`
	CRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	CRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	SHA1      string `xml:"ChecksumSHA1,omitempty"`
	SHA256    string `xml:"ChecksumSHA256,omitempty"`
	Type      string `xml:"ChecksumType,omitempty"`
}

type objectAttributesParts struct {
	IsTruncated          bool                   `xml:"IsTruncated"`
	MaxParts             int                    `xml:"MaxParts"`
	NextPartNumberMarker int                    `xml:"NextPartNumberMarker,omitempty"`
	PartNumberMarker     int                    `xml:"PartNumberMarker"`
	Parts                []objectAttributesPart `xml:"Part,omitempty"`
	PartsCount           int                    `xml:"PartsCount"`
}

type objectAttributesPart struct {
	CRC32     string `xml:"ChecksumCRC32,omitempty"`
	CRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	CRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	SHA1      string `xml:"ChecksumSHA1,omitempty"`
	SHA256    string `xml:"ChecksumSHA256,omitempty"`
	Number    int    `xml:"PartNumber"`
	Size      int64  `xml:"Size"`
}

func (h *S3Handler) GetObjectAttributes(c *gin.Context) {
	observation := newObjectReadObservation("GetObjectAttributes", objectReadModeAttributes)
	defer observation.finish(c)
	bucket, key, apiError := h.authorizeObject(c, false)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	attributes, marker, maxParts, apiError := parseObjectAttributesRequest(c.Request)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	observation.setAttributes(attributes)
	if err := validateHistoricalObjectKeyBoundary(bucket.Name, key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	objectPath := "/" + bucket.Name + "/" + key
	info, err := h.fmgr.StatS3Object(c.Request.Context(), objectPath)
	if err != nil {
		s3base.WriteError(c, objectError(err, bucket.Name, key, objectPath))
		return
	}
	response, apiError := h.buildObjectAttributesResponse(
		c,
		info,
		attributes,
		marker,
		maxParts,
		observation,
	)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	c.Header("Last-Modified", time.UnixMilli(info.Link.Mtime).UTC().Format(http.TimeFormat))
	c.XML(http.StatusOK, response)
}

func parseObjectAttributesRequest(
	request *http.Request,
) (objectAttributeSet, int, int, *s3base.APIError) {
	query := request.URL.Query()
	values := query["attributes"]
	if len(values) != 1 || values[0] != "" {
		return objectAttributeSet{}, 0, 0, s3base.InvalidRequest(
			"attributes must be specified once with an empty value.",
			nil,
		)
	}
	if len(query["x-id"]) > 1 {
		return objectAttributeSet{}, 0, 0, invalidReadArgument(
			"x-id was specified more than once.",
			nil,
		)
	}
	for name := range query {
		if name == "attributes" || name == "x-id" {
			continue
		}
		switch name {
		case "uploadId", "uploads", "partNumber",
			"response-cache-control", "response-content-disposition",
			"response-content-encoding", "response-content-language",
			"response-content-type", "response-expires":
			return objectAttributeSet{}, 0, 0, s3base.InvalidRequest(
				"attributes cannot be combined with another object operation.",
				nil,
			)
		case "versionId":
			return objectAttributeSet{}, 0, 0, s3base.NewError(
				http.StatusNotImplemented,
				"NotImplemented",
				"Object versioning is not implemented.",
				nil,
			)
		}
		if hasUnsupportedObjectReadQuery(map[string][]string{name: query[name]}) {
			return objectAttributeSet{}, 0, 0, s3base.NewError(
				http.StatusNotImplemented,
				"NotImplemented",
				"The requested object subresource is not implemented.",
				nil,
			)
		}
	}
	rawAttributes, apiError := requiredObjectAttributesHeader(request.Header)
	if apiError != nil {
		return objectAttributeSet{}, 0, 0, apiError
	}
	attributes, apiError := parseObjectAttributeSet(rawAttributes)
	if apiError != nil {
		return objectAttributeSet{}, 0, 0, apiError
	}
	maxParts, apiError := parseObjectAttributePageHeader(
		request.Header,
		"X-Amz-Max-Parts",
		defaultObjectAttributesMaxParts,
		0,
		1000,
	)
	if apiError != nil {
		return objectAttributeSet{}, 0, 0, apiError
	}
	marker, apiError := parseObjectAttributePageHeader(
		request.Header,
		"X-Amz-Part-Number-Marker",
		0,
		0,
		maxMultipartPartNumber,
	)
	if apiError != nil {
		return objectAttributeSet{}, 0, 0, apiError
	}
	return attributes, marker, maxParts, nil
}

func requiredObjectAttributesHeader(header http.Header) (string, *s3base.APIError) {
	values := header.Values("X-Amz-Object-Attributes")
	if len(values) == 0 {
		return "", s3base.InvalidRequest("x-amz-object-attributes is required.", nil)
	}
	if len(values) != 1 {
		return "", invalidReadArgument(
			"x-amz-object-attributes was specified more than once.",
			nil,
		)
	}
	value := values[0]
	if value == "" {
		return "", s3base.InvalidRequest("x-amz-object-attributes is required.", nil)
	}
	return value, nil
}

func parseObjectAttributeSet(value string) (objectAttributeSet, *s3base.APIError) {
	var result objectAttributeSet
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(value, ",") {
		name := strings.Trim(raw, " \t")
		if name == "" {
			return objectAttributeSet{}, invalidReadArgument(
				"x-amz-object-attributes contains an empty value.",
				nil,
			)
		}
		if _, duplicate := seen[name]; duplicate {
			return objectAttributeSet{}, invalidReadArgument(
				"x-amz-object-attributes contains a duplicate value.",
				nil,
			)
		}
		seen[name] = struct{}{}
		switch name {
		case "ETag":
			result.etag = true
		case "Checksum":
			result.checksum = true
		case "ObjectParts":
			result.objectParts = true
		case "StorageClass":
			result.storageClass = true
		case "ObjectSize":
			result.objectSize = true
		default:
			return objectAttributeSet{}, invalidReadArgument(
				"x-amz-object-attributes contains an unsupported value.",
				nil,
			)
		}
	}
	return result, nil
}

func parseObjectAttributePageHeader(
	header http.Header,
	name string,
	defaultValue int,
	minimum int,
	maximum int,
) (int, *s3base.APIError) {
	values := header.Values(name)
	if len(values) == 0 {
		return defaultValue, nil
	}
	if len(values) != 1 {
		return 0, invalidReadArgument(name+" was specified more than once.", nil)
	}
	value := values[0]
	parsed, err := parseBoundedDecimal(value, minimum, maximum)
	if err != nil {
		return 0, invalidReadArgument(name+" is invalid.", err)
	}
	return parsed, nil
}

func (h *S3Handler) buildObjectAttributesResponse(
	c *gin.Context,
	info *filemgr.S3ObjectInfo,
	attributes objectAttributeSet,
	marker int,
	maxParts int,
	observation *objectReadObservation,
) (*getObjectAttributesResponse, *s3base.APIError) {
	response := &getObjectAttributesResponse{
		XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	if attributes.etag {
		response.ETag = &info.Metadata.ETag
	}
	if attributes.checksum {
		checksum, exists, err := objectMetadataChecksum(info.Metadata)
		if err != nil {
			return nil, s3base.InternalError(err)
		}
		if exists {
			response.Checksum = checksum
		}
	}
	if attributes.storageClass {
		response.StorageClass = objectStorageClassStandard
	}
	if attributes.objectSize {
		size := info.Link.FileSize
		response.ObjectSize = &size
	}
	if attributes.objectParts {
		if apiError := h.populateObjectAttributeParts(
			c,
			info,
			marker,
			maxParts,
			response,
			observation,
		); apiError != nil {
			return nil, apiError
		}
	}
	return response, nil
}

func (h *S3Handler) populateObjectAttributeParts(
	c *gin.Context,
	info *filemgr.S3ObjectInfo,
	marker int,
	maxParts int,
	response *getObjectAttributesResponse,
	observation *objectReadObservation,
) *s3base.APIError {
	page, err := h.fmgr.ListS3ObjectParts(
		c.Request.Context(),
		info.Link.FileId,
		info.Link.FileSize,
		marker,
		maxParts,
	)
	if err != nil {
		return s3base.InternalError(err)
	}
	if !page.IsMultipart {
		return nil
	}
	observation.partPageSize = len(page.Parts)
	observation.partPageTruncated = page.IsTruncated
	observation.manifestChecksumState = completedPartChecksumState(page.Parts)
	parts, err := objectAttributeParts(info.Metadata, page.Parts)
	if err != nil {
		return s3base.InternalError(err)
	}
	response.ObjectParts = &objectAttributesParts{
		IsTruncated:          page.IsTruncated,
		MaxParts:             page.MaxParts,
		NextPartNumberMarker: page.NextPartNumberMarker,
		PartNumberMarker:     page.PartNumberMarker,
		Parts:                parts,
		PartsCount:           page.PartsCount,
	}
	return nil
}

func objectMetadataChecksum(
	metadata *entity.S3ObjectMetadata,
) (*objectAttributesChecksum, bool, error) {
	if metadata.ChecksumSHA256 == "" && metadata.RequestChecksumAlgorithm == "" {
		return nil, false, nil
	}
	result := &objectAttributesChecksum{SHA256: metadata.ChecksumSHA256}
	if metadata.RequestChecksumAlgorithm != "" {
		if metadata.RequestChecksumValue == "" {
			return nil, false, errObjectChecksumValueMissing
		}
		if err := setObjectAttributeChecksum(
			result,
			metadata.RequestChecksumAlgorithm,
			metadata.RequestChecksumValue,
		); err != nil {
			return nil, false, err
		}
	}
	result.Type = metadata.ChecksumType
	return result, true, nil
}

func setObjectAttributeChecksum(
	checksum *objectAttributesChecksum,
	algorithm string,
	value string,
) error {
	switch algorithm {
	case "CRC32":
		checksum.CRC32 = value
	case "CRC32C":
		checksum.CRC32C = value
	case "CRC64NVME":
		checksum.CRC64NVME = value
	case "SHA1":
		checksum.SHA1 = value
	case "SHA256":
		if checksum.SHA256 != "" && checksum.SHA256 != value {
			return errObjectSHA256Inconsistent
		}
		checksum.SHA256 = value
	default:
		return errObjectChecksumUnsupported
	}
	return nil
}

func objectAttributeParts(
	metadata *entity.S3ObjectMetadata,
	parts []entity.S3CompletedPart,
) ([]objectAttributesPart, error) {
	if metadata.RequestChecksumAlgorithm == "" {
		for _, part := range parts {
			if part.ChecksumState == "available" {
				return nil, errPartChecksumWithoutObjectPolicy
			}
		}
		return []objectAttributesPart{}, nil
	}
	result := make([]objectAttributesPart, 0, len(parts))
	for _, part := range parts {
		item := objectAttributesPart{Number: part.PartNumber, Size: part.PartSize}
		if part.ChecksumState == "available" {
			if part.ChecksumAlgorithm != metadata.RequestChecksumAlgorithm {
				return nil, errPartChecksumAlgorithmMismatch
			}
			checksum := &objectAttributesChecksum{}
			if err := setObjectAttributeChecksum(
				checksum,
				part.ChecksumAlgorithm,
				part.ChecksumValue,
			); err != nil {
				return nil, err
			}
			item.CRC32 = checksum.CRC32
			item.CRC32C = checksum.CRC32C
			item.CRC64NVME = checksum.CRC64NVME
			item.SHA1 = checksum.SHA1
			item.SHA256 = checksum.SHA256
		}
		result = append(result, item)
	}
	return result, nil
}
