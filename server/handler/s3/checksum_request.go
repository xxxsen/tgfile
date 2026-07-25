package s3

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/xxxsen/tgfile/s3checksum"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
)

var (
	errEmptyNonNegativeInteger   = errors.New("empty non-negative integer")
	errInvalidNonNegativeInteger = errors.New("invalid non-negative integer")
)

type checksumRequest struct {
	algorithm   s3checksum.Algorithm
	headerValue string
	trailerName string
}

func parseChecksumRequest(request *http.Request) (*checksumRequest, *s3base.APIError) {
	return parseChecksumRequestAllowType(request, false)
}

func parseChecksumRequestAllowType(
	request *http.Request,
	allowType bool,
) (*checksumRequest, *s3base.APIError) {
	selected, apiError := parseChecksumHeader(request, allowType)
	if apiError != nil {
		return nil, apiError
	}
	if apiError := mergeChecksumTrailer(request, selected); apiError != nil {
		return nil, apiError
	}
	if apiError := validateSDKChecksumAlgorithm(request, selected); apiError != nil {
		return nil, apiError
	}
	return selected, nil
}

func parseChecksumHeader(
	request *http.Request,
	allowType bool,
) (*checksumRequest, *s3base.APIError) {
	selected := &checksumRequest{}
	for name := range request.Header {
		lowerName := strings.ToLower(name)
		if !strings.HasPrefix(lowerName, "x-amz-checksum-") {
			continue
		}
		suffix := strings.TrimPrefix(lowerName, "x-amz-checksum-")
		if suffix == "type" && allowType {
			continue
		}
		if suffix == "type" || suffix == "algorithm" {
			return nil, s3base.InvalidRequest("The checksum header is not valid for this operation.", nil)
		}
		algorithm, apiError := parseChecksumAlgorithm(strings.ToUpper(suffix))
		if apiError != nil {
			return nil, apiError
		}
		value, apiError := singletonHeader(request.Header, name)
		if apiError != nil {
			return nil, apiError
		}
		if selected.algorithm != "" && selected.algorithm != algorithm {
			return nil, s3base.InvalidRequest("Only one additional checksum algorithm is allowed.", nil)
		}
		if _, err := s3checksum.Decode(algorithm, value); err != nil {
			return nil, invalidChecksumDigest(err)
		}
		selected.algorithm = algorithm
		selected.headerValue = value
	}
	return selected, nil
}

func mergeChecksumTrailer(
	request *http.Request,
	selected *checksumRequest,
) *s3base.APIError {
	trailerAlgorithm, trailerName, apiError := parseChecksumTrailer(request)
	if apiError != nil {
		return apiError
	}
	if trailerAlgorithm != "" {
		if selected.algorithm != "" && selected.algorithm != trailerAlgorithm {
			return s3base.InvalidRequest(
				"The checksum header and trailer must use the same algorithm.",
				nil,
			)
		}
		selected.algorithm = trailerAlgorithm
		selected.trailerName = trailerName
	}
	return nil
}

func validateSDKChecksumAlgorithm(
	request *http.Request,
	selected *checksumRequest,
) *s3base.APIError {
	sdkValue, apiError := optionalSingletonHeader(request.Header, "X-Amz-Sdk-Checksum-Algorithm")
	if apiError != nil {
		return apiError
	}
	if sdkValue != "" {
		sdkAlgorithm, apiError := parseChecksumAlgorithm(sdkValue)
		if apiError != nil {
			return apiError
		}
		if selected.algorithm == "" || selected.algorithm != sdkAlgorithm {
			return s3base.InvalidRequest(
				"x-amz-sdk-checksum-algorithm requires a matching checksum header or trailer.",
				nil,
			)
		}
	}
	return nil
}

func parseCreateMultipartChecksum(
	request *http.Request,
) (s3checksum.Algorithm, s3checksum.Type, *s3base.APIError) {
	if request.Header.Get("X-Amz-Trailer") != "" ||
		request.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" {
		return "", "", s3base.InvalidRequest(
			"CreateMultipartUpload does not accept checksum trailers or SDK algorithm headers.",
			nil,
		)
	}
	for name := range request.Header {
		lowerName := strings.ToLower(name)
		if !strings.HasPrefix(lowerName, "x-amz-checksum-") {
			continue
		}
		if lowerName != "x-amz-checksum-algorithm" && lowerName != "x-amz-checksum-type" {
			return "", "", s3base.InvalidRequest(
				"CreateMultipartUpload only accepts checksum algorithm and type headers.",
				nil,
			)
		}
	}
	algorithmValue, apiError := optionalSingletonHeader(
		request.Header,
		"X-Amz-Checksum-Algorithm",
	)
	if apiError != nil {
		return "", "", apiError
	}
	typeValue, apiError := optionalSingletonHeader(request.Header, "X-Amz-Checksum-Type")
	if apiError != nil {
		return "", "", apiError
	}
	algorithm, checksumType, err := s3checksum.ResolveMultipart(algorithmValue, typeValue)
	if err == nil {
		return algorithm, checksumType, nil
	}
	switch {
	case errors.Is(err, s3checksum.ErrUnsupportedAlgorithm):
		return "", "", multipartNotImplemented("The requested checksum algorithm is not implemented.")
	case errors.Is(err, s3checksum.ErrInvalidAlgorithm):
		return "", "", invalidMultipartArgument("The checksum algorithm is invalid.", err)
	default:
		return "", "", s3base.InvalidRequest(
			"The checksum algorithm and type combination is invalid.",
			err,
		)
	}
}

func validateChecksumMode(request *http.Request) *s3base.APIError {
	value, apiError := optionalSingletonHeader(request.Header, "X-Amz-Checksum-Mode")
	if apiError != nil {
		return apiError
	}
	if value != "" && value != "ENABLED" {
		return invalidMultipartArgument("x-amz-checksum-mode must be ENABLED.", nil)
	}
	return nil
}

type completeChecksumHeaders struct {
	algorithm    s3checksum.Algorithm
	value        string
	checksumType s3checksum.Type
	expectedSize *int64
}

func parseCompleteChecksumHeaders(
	request *http.Request,
) (*completeChecksumHeaders, *s3base.APIError) {
	checksum, apiError := parseChecksumRequestAllowType(request, true)
	if apiError != nil {
		return nil, apiError
	}
	if checksum.trailerName != "" || request.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" {
		return nil, s3base.InvalidRequest(
			"CompleteMultipartUpload does not accept checksum trailers or SDK algorithm headers.",
			nil,
		)
	}
	typeValue, apiError := optionalSingletonHeader(request.Header, "X-Amz-Checksum-Type")
	if apiError != nil {
		return nil, apiError
	}
	var checksumType s3checksum.Type
	if typeValue != "" {
		var err error
		checksumType, err = s3checksum.ParseType(typeValue)
		if err != nil {
			return nil, s3base.InvalidRequest("The checksum type is invalid.", err)
		}
	}
	sizeValue, apiError := optionalSingletonHeader(request.Header, "X-Amz-Mp-Object-Size")
	if apiError != nil {
		return nil, apiError
	}
	var expectedSize *int64
	if sizeValue != "" {
		parsed, err := parseNonNegativeInt64(sizeValue)
		if err != nil {
			return nil, s3base.InvalidRequest("x-amz-mp-object-size is invalid.", err)
		}
		expectedSize = &parsed
	}
	return &completeChecksumHeaders{
		algorithm:    checksum.algorithm,
		value:        checksum.headerValue,
		checksumType: checksumType,
		expectedSize: expectedSize,
	}, nil
}

func parseChecksumTrailer(
	request *http.Request,
) (s3checksum.Algorithm, string, *s3base.APIError) {
	declared, apiError := optionalSingletonHeader(request.Header, "X-Amz-Trailer")
	if apiError != nil || declared == "" {
		return "", "", apiError
	}
	names := strings.Split(declared, ",")
	if len(names) != 1 {
		return "", "", s3base.InvalidRequest(
			"x-amz-trailer must declare exactly one supported checksum.",
			nil,
		)
	}
	name := strings.ToLower(strings.TrimSpace(names[0]))
	if !strings.HasPrefix(name, "x-amz-checksum-") {
		return "", "", s3base.InvalidRequest(
			"x-amz-trailer must declare a supported checksum.",
			nil,
		)
	}
	algorithm, apiError := parseChecksumAlgorithm(
		strings.ToUpper(strings.TrimPrefix(name, "x-amz-checksum-")),
	)
	if apiError != nil {
		return "", "", apiError
	}
	return algorithm, name, nil
}

func parseChecksumAlgorithm(value string) (s3checksum.Algorithm, *s3base.APIError) {
	algorithm, err := s3checksum.ParseAlgorithm(value)
	if err == nil {
		return algorithm, nil
	}
	if errors.Is(err, s3checksum.ErrUnsupportedAlgorithm) {
		return "", multipartNotImplemented("The requested checksum algorithm is not implemented.")
	}
	return "", invalidMultipartArgument("The checksum algorithm is invalid.", err)
}

func singletonHeader(header http.Header, name string) (string, *s3base.APIError) {
	value, apiError := optionalSingletonHeader(header, name)
	if apiError != nil {
		return "", apiError
	}
	if value == "" {
		return "", s3base.InvalidRequest("A checksum header must not be empty.", nil)
	}
	return value, nil
}

func optionalSingletonHeader(header http.Header, name string) (string, *s3base.APIError) {
	values := header.Values(name)
	if len(values) == 0 {
		return "", nil
	}
	value := values[0]
	for _, candidate := range values[1:] {
		if candidate != value {
			return "", s3base.InvalidRequest("A singleton header has conflicting values.", nil)
		}
	}
	return value, nil
}

func invalidChecksumDigest(cause error) *s3base.APIError {
	return s3base.NewError(
		http.StatusBadRequest,
		"InvalidDigest",
		"The request checksum is invalid.",
		cause,
	)
}

func parseNonNegativeInt64(value string) (int64, error) {
	if value == "" {
		return 0, errEmptyNonNegativeInteger
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%w: %q", errInvalidNonNegativeInteger, value)
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse non-negative integer: %w", err)
	}
	return parsed, nil
}
