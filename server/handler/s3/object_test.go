package s3

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
)

func TestValidateNewObjectKey(t *testing.T) {
	valid := []string{"a", "dir/object.bin", "报告/数据.txt"}
	for _, key := range valid {
		require.NoError(t, validateNewObjectKey(key), key)
	}
	invalid := []string{"", "/a", "a/", "a//b", "a/./b", "a/../b", "a\\b", "a\x00b"}
	for _, key := range invalid {
		require.ErrorIs(t, validateNewObjectKey(key), errInvalidObjectName, key)
	}
}

func TestHistoricalObjectKeyCannotEscapeBucket(t *testing.T) {
	valid := []string{"a", "dir/object.bin", "dir/ object "}
	for _, key := range valid {
		require.NoError(t, validateHistoricalObjectKeyBoundary("bucket", key), key)
	}
	invalid := []string{"", "/a", "a/", "a//b", "a/./b", "a/../private/object"}
	for _, key := range invalid {
		require.ErrorIs(
			t,
			validateHistoricalObjectKeyBoundary("bucket", key),
			errInvalidObjectName,
			key,
		)
	}
}

func TestRequestMetadataNormalizesUserMetadata(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/bucket/a.txt", nil)
	request.Header.Add("X-Amz-Meta-Team", "one")
	request.Header.Add("X-Amz-Meta-Team", "two")
	request.Header.Set("Expires", "Sun, 06 Nov 1994 08:49:37 GMT")

	metadata, apiError := parseRequestMetadata(request, "a.txt")

	require.Nil(t, apiError)
	require.Equal(t, "text/plain", metadata.ContentType)
	require.Equal(t, `{"team":"one,two"}`, metadata.UserMetadata)
	require.Equal(t, "Sun, 06 Nov 1994 08:49:37 GMT", metadata.Expires)
}

func TestCRC64NVMEVector(t *testing.T) {
	checksum, err := s3checksum.NewHash(s3checksum.AlgorithmCRC64NVME)
	require.NoError(t, err)
	_, err = checksum.Write([]byte("hello world"))
	require.NoError(t, err)
	require.Equal(t, "jSnVw/bqjr4=", base64.StdEncoding.EncodeToString(checksum.Sum(nil)))
}

func TestUploadChecksumTrailerIsVerifiedAgainstObject(t *testing.T) {
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/private/object",
		bytes.NewBufferString("payload"),
	)
	request.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
	request.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "SHA256")
	hashes, reader, apiError := newUploadHashes(request)
	require.Nil(t, apiError)
	_, err := io.Copy(io.Discard, reader)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Set("s3-verified-trailers", http.Header{
		"X-Amz-Checksum-Sha256": []string{"I59Z7VXnN8dxR89VrQwbAwttfudIp0JpUvm4UtWpNeU="},
	})

	require.Nil(t, hashes.loadTrailer(context))
	require.Nil(t, hashes.validate())
}

func TestUploadChecksumTrailerRejectsMismatchedObject(t *testing.T) {
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/private/object",
		bytes.NewBufferString("payload"),
	)
	request.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	hashes, reader, apiError := newUploadHashes(request)
	require.Nil(t, apiError)
	_, err := io.Copy(io.Discard, reader)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Set("s3-verified-trailers", http.Header{
		"X-Amz-Checksum-Crc32": []string{"AAAAAA=="},
	})

	require.Nil(t, hashes.loadTrailer(context))
	apiError = hashes.validate()
	require.NotNil(t, apiError)
	require.Equal(t, "BadDigest", apiError.Code)
}

func TestUploadChecksumHeaderAndTrailerMustMatch(t *testing.T) {
	const payloadChecksum = "I59Z7VXnN8dxR89VrQwbAwttfudIp0JpUvm4UtWpNeU="
	for _, test := range []struct {
		name         string
		trailerValue string
		wantCode     string
	}{
		{name: "matching", trailerValue: payloadChecksum},
		{
			name:         "mismatching",
			trailerValue: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			wantCode:     "BadDigest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPut,
				"/private/object",
				bytes.NewBufferString("payload"),
			)
			request.Header.Set("X-Amz-Checksum-Sha256", payloadChecksum)
			request.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
			request.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "SHA256")
			hashes, reader, apiError := newUploadHashes(request)
			require.Nil(t, apiError)
			_, err := io.Copy(io.Discard, reader)
			require.NoError(t, err)

			gin.SetMode(gin.TestMode)
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Set("s3-verified-trailers", http.Header{
				"X-Amz-Checksum-Sha256": []string{test.trailerValue},
			})

			apiError = hashes.loadTrailer(context)
			if test.wantCode != "" {
				require.NotNil(t, apiError)
				require.Equal(t, test.wantCode, apiError.Code)
				return
			}
			require.Nil(t, apiError)
			require.Nil(t, hashes.validate())
		})
	}
}

func TestAWSChunkedEncodingRequiresVerifiedStreamingPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/bucket/object", nil)
	request.Header.Set("Content-Encoding", "gzip, aws-chunked")
	context.Request = request

	apiError := validateAWSChunkedUpload(context)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidRequest", apiError.Code)

	context.Set("s3-decoded-content-length", int64(0))
	require.Nil(t, validateAWSChunkedUpload(context))
}

func TestPublicReadRejectsInvalidPresentedCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewS3Handler(nil, Config{
		Buckets: []Bucket{{Name: "public", ACL: BucketACLPublicRead}},
		Users:   map[string]string{"access": "secret"},
	})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/public/object", bytes.NewReader(nil))
	request.SetBasicAuth("access", "wrong")
	context.Request = request

	_, apiError := handler.Authorize(context, false)

	require.NotNil(t, apiError)
	require.Equal(t, http.StatusForbidden, apiError.HTTPStatus)
	require.Equal(t, "AccessDenied", apiError.Code)
}

func TestPublicReadAllowsTrulyAnonymousRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewS3Handler(nil, Config{
		Buckets: []Bucket{{Name: "public", ACL: BucketACLPublicRead}},
	})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/public/object", nil)

	result, apiError := handler.Authorize(context, false)

	require.Nil(t, result)
	require.Nil(t, apiError)
}

func TestBasicAuthorizationReturnsIdentityAndChecksPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authorizer, err := authz.New(map[string][]string{
		"reader": {string(authz.S3Read)},
	})
	require.NoError(t, err)
	handler := NewS3Handler(nil, Config{
		Buckets:    []Bucket{{Name: "public", ACL: BucketACLPublicRead}},
		Users:      map[string]string{"reader": "secret"},
		Authorizer: authorizer,
	})

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/public/object", nil)
	request.SetBasicAuth("reader", "secret")
	context.Request = request
	identity, apiError := handler.Authorize(context, false, authz.S3Read)
	require.Nil(t, apiError)
	require.Equal(t, "reader", identity.Username)

	identity, apiError = handler.Authorize(context, true, authz.S3Write)
	require.Nil(t, identity)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusForbidden, apiError.HTTPStatus)
}

func TestReadDateConditionsUseSecondPrecision(t *testing.T) {
	base := time.Date(2026, time.July, 25, 12, 0, 1, 0, time.UTC)
	info := &filemgr.S3ObjectInfo{
		Link: &entity.FileLinkMeta{Mtime: base.UnixMilli()},
		Metadata: &entity.S3ObjectMetadata{
			ETag: `"current"`,
		},
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/bucket/object", nil)
	request.Header.Set("If-Unmodified-Since", base.Add(-time.Second).Format(http.TimeFormat))
	apiError := checkReadConditions(request, info)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusPreconditionFailed, apiError.HTTPStatus)

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/bucket/object", nil)
	request.Header.Set("If-Modified-Since", base.Add(-time.Second).Format(http.TimeFormat))
	require.Nil(t, checkReadConditions(request, info))

	info.Link.Mtime = base.Add(999 * time.Millisecond).UnixMilli()
	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/bucket/object", nil)
	request.Header.Set("If-Modified-Since", base.Format(http.TimeFormat))
	apiError = checkReadConditions(request, info)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusNotModified, apiError.HTTPStatus)
}

func TestObjectReadOptionsValidateOverridesAndNormalizeHTTPDates(t *testing.T) {
	expected := time.Date(1994, time.November, 6, 8, 49, 37, 0, time.UTC)
	for _, date := range []string{
		expected.Format(http.TimeFormat),
		"Sunday, 06-Nov-94 08:49:37 GMT",
		expected.Format(time.ANSIC),
	} {
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/bucket/object?response-expires="+url.QueryEscape(date),
			nil,
		)
		options, apiError := parseObjectReadOptions(request)
		require.Nil(t, apiError, date)
		require.Equal(t, expected.Format(http.TimeFormat), options.responseOverride["Expires"])
	}

	for queryName := range responseOverrideHeaders {
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/bucket/object?"+queryName+"=",
			nil,
		)
		_, apiError := parseObjectReadOptions(request)
		require.NotNil(t, apiError, queryName)
		require.Equal(t, "InvalidArgument", apiError.Code)
	}

	for _, control := range []byte{0, '\r', '\n', 0x1f, 0x7f} {
		query := url.Values{"response-content-type": {string([]byte{'a', control, 'b'})}}
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/bucket/object?"+query.Encode(),
			nil,
		)
		_, apiError := parseObjectReadOptions(request)
		require.NotNil(t, apiError, control)
		require.Equal(t, "InvalidArgument", apiError.Code)
	}
}

func TestObjectAttributeRequestParsingIsStrictAndBounded(t *testing.T) {
	for _, name := range []string{"ETag", "Checksum", "ObjectParts", "StorageClass", "ObjectSize"} {
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/bucket/object?attributes",
			nil,
		)
		request.Header.Set("X-Amz-Object-Attributes", name)
		_, marker, maxParts, apiError := parseObjectAttributesRequest(request)
		require.Nil(t, apiError, name)
		require.Zero(t, marker)
		require.Equal(t, 1000, maxParts)
	}

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/bucket/object?attributes",
		nil,
	)
	request.Header.Set(
		"X-Amz-Object-Attributes",
		" ETag , Checksum , ObjectParts , StorageClass , ObjectSize ",
	)
	request.Header.Set("X-Amz-Max-Parts", "0")
	request.Header.Set("X-Amz-Part-Number-Marker", "10000")
	attributes, marker, maxParts, apiError := parseObjectAttributesRequest(request)
	require.Nil(t, apiError)
	require.True(t, attributes.etag)
	require.True(t, attributes.checksum)
	require.True(t, attributes.objectParts)
	require.True(t, attributes.storageClass)
	require.True(t, attributes.objectSize)
	require.Equal(t, 10000, marker)
	require.Zero(t, maxParts)

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/bucket/object?attributes",
		nil,
	)
	request.Header.Add("X-Amz-Object-Attributes", "ETag")
	request.Header.Add("X-Amz-Object-Attributes", "ObjectSize")
	parsedAttributes, parsedMarker, parsedMaxParts, apiError := parseObjectAttributesRequest(request)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidArgument", apiError.Code)
	require.Equal(t, objectAttributeSet{}, parsedAttributes)
	require.Zero(t, parsedMarker)
	require.Zero(t, parsedMaxParts)

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/bucket/object?attributes",
		nil,
	)
	request.Header.Add("X-Amz-Object-Attributes", "ETag")
	request.Header.Add("X-Amz-Object-Attributes", "ETag")
	parsedAttributes, parsedMarker, parsedMaxParts, apiError = parseObjectAttributesRequest(request)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidArgument", apiError.Code)
	require.Equal(t, objectAttributeSet{}, parsedAttributes)
	require.Zero(t, parsedMarker)
	require.Zero(t, parsedMaxParts)

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/bucket/object?attributes",
		nil,
	)
	request.Header.Set("X-Amz-Object-Attributes", "ETag")
	request.Header.Add("X-Amz-Max-Parts", "1")
	request.Header.Add("X-Amz-Max-Parts", "2")
	parsedAttributes, parsedMarker, parsedMaxParts, apiError = parseObjectAttributesRequest(request)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidArgument", apiError.Code)
	require.Equal(t, objectAttributeSet{}, parsedAttributes)
	require.Zero(t, parsedMarker)
	require.Zero(t, parsedMaxParts)

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/bucket/object?attributes",
		nil,
	)
	request.Header.Set("X-Amz-Object-Attributes", "ETag")
	request.Header.Add("X-Amz-Max-Parts", "1")
	request.Header.Add("X-Amz-Max-Parts", "1")
	parsedAttributes, parsedMarker, parsedMaxParts, apiError = parseObjectAttributesRequest(request)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidArgument", apiError.Code)
	require.Equal(t, objectAttributeSet{}, parsedAttributes)
	require.Zero(t, parsedMarker)
	require.Zero(t, parsedMaxParts)
}
