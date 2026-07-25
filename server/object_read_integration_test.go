package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
)

func TestS3ResponseHeaderOverridesAuthenticationAndStatusBoundaries(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	objectURL := environment.server.URL + "/hackmd/read-options.txt"
	content := []byte("response override content")
	put := authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader(content))
	put.Header.Set("Content-Type", "text/plain")
	response, err := client.Do(put)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, content, readResponse(t, response))

	overrideValues := url.Values{
		"response-cache-control":       {"private, max-age=60"},
		"response-content-disposition": {`attachment; filename="download.txt"`},
		"response-content-encoding":    {"identity"},
		"response-content-language":    {"en-US"},
		"response-content-type":        {"application/octet-stream"},
		"response-expires":             {"Sunday, 06-Nov-94 08:49:37 GMT"},
	}
	target := objectURL + "?" + overrideValues.Encode()
	response, err = getResponse(t, client, target)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NotEqual(t, "application/octet-stream", response.Header.Get("Content-Type"))
	_ = readResponse(t, response)

	request := authenticatedRequest(t, http.MethodGet, target, nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "private, max-age=60", response.Header.Get("Cache-Control"))
	require.Equal(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, "identity", response.Header.Get("Content-Encoding"))
	require.Equal(t, "en-US", response.Header.Get("Content-Language"))
	require.Equal(t, "application/octet-stream", response.Header.Get("Content-Type"))
	require.Equal(t, "Sun, 06 Nov 1994 08:49:37 GMT", response.Header.Get("Expires"))
	require.Equal(t, content, readResponse(t, response))

	request = signedHeaderRequest(t, http.MethodGet, target, nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, content, readResponse(t, response))

	request = authenticatedRequest(t, http.MethodHead, target, nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "private, max-age=60", response.Header.Get("Cache-Control"))
	require.Equal(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, "identity", response.Header.Get("Content-Encoding"))
	require.Equal(t, "en-US", response.Header.Get("Content-Language"))
	require.Equal(t, "application/octet-stream", response.Header.Get("Content-Type"))
	require.Equal(t, "Sun, 06 Nov 1994 08:49:37 GMT", response.Header.Get("Expires"))
	require.Equal(t, strconv.Itoa(len(content)), response.Header.Get("Content-Length"))
	_ = readResponse(t, response)

	request = authenticatedRequest(t, http.MethodGet, target, nil)
	request.Header.Set("Range", "bytes=0-3")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.NotEqual(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, content[:4], readResponse(t, response))

	request = authenticatedRequest(t, http.MethodGet, target, nil)
	request.Header.Set("If-None-Match", "*")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotModified, response.StatusCode)
	require.NotEqual(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	_ = readResponse(t, response)

	request = authenticatedRequest(t, http.MethodGet, target, nil)
	request.Header.Set("If-Match", `"not-the-current-etag"`)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
	require.NotEqual(t, `attachment; filename="download.txt"`, response.Header.Get("Content-Disposition"))
	_ = readResponse(t, response)

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+
			"/hackmd/missing.txt?response-content-disposition=private-value",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NotEqual(t, "private-value", response.Header.Get("Content-Disposition"))
	_ = readResponse(t, response)

	for _, invalidTarget := range []string{
		objectURL + "?response-content-type=",
		objectURL + "?response-content-type=text/plain&response-content-type=application/json",
		objectURL + "?response-content-type=text%0Aplain",
		objectURL + "?response-expires=not-a-date",
		objectURL + "?x-id=GetObject&x-id=GetObject",
	} {
		request = authenticatedRequest(t, http.MethodGet, invalidTarget, nil)
		response, err = client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, response.StatusCode, invalidTarget)
		require.Contains(t, string(readResponse(t, response)), "InvalidArgument")
	}

	boundaryValue := strings.Repeat("a", 8192)
	request = authenticatedRequest(
		t,
		http.MethodGet,
		objectURL+"?response-cache-control="+boundaryValue,
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Len(t, response.Header.Get("Cache-Control"), 8192)
	_ = readResponse(t, response)
	request = authenticatedRequest(
		t,
		http.MethodGet,
		objectURL+"?response-cache-control="+boundaryValue+"a",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	_ = readResponse(t, response)

	assertPresignedResponseOverride(t, client, objectURL, content)
}

func signedHeaderRequest(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	requestBody := bytes.NewReader(body)
	request, err := http.NewRequestWithContext(t.Context(), method, target, requestBody)
	require.NoError(t, err)
	payloadDigest := sha256.Sum256(body)
	digest := hex.EncodeToString(payloadDigest[:])
	request.Header.Set("X-Amz-Content-Sha256", digest)
	signer := awsv4.NewSigner()
	require.NoError(t, signer.SignHTTP(
		t.Context(),
		aws.Credentials{AccessKeyID: "access", SecretAccessKey: "secret"},
		request,
		digest,
		"s3",
		"us-east-1",
		time.Now(),
	))
	return request
}

func assertPresignedResponseOverride(
	t *testing.T,
	client *http.Client,
	objectURL string,
	content []byte,
) {
	t.Helper()
	credentials := aws.Credentials{AccessKeyID: "access", SecretAccessKey: "secret"}
	signer := awsv4.NewSigner()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	query := request.URL.Query()
	query.Set("response-content-disposition", `attachment; filename="signed.bin"`)
	query.Set("X-Amz-Expires", "300")
	request.URL.RawQuery = query.Encode()
	signedURL, signedHeaders, err := signer.PresignHTTP(
		t.Context(),
		credentials,
		request,
		"UNSIGNED-PAYLOAD",
		"s3",
		"us-east-1",
		time.Now(),
	)
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, signedURL, nil)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, `attachment; filename="signed.bin"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, content, readResponse(t, response))

	tamperedURL, err := url.Parse(signedURL)
	require.NoError(t, err)
	query = tamperedURL.Query()
	query.Set("response-content-disposition", `attachment; filename="tampered.bin"`)
	tamperedURL.RawQuery = query.Encode()
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, tamperedURL.String(), nil)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "SignatureDoesNotMatch")
}

func TestS3PartNumberAndGetObjectAttributes(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()

	ordinaryURL := environment.server.URL + "/hackmd/ordinary-large.bin"
	ordinaryContent := bytes.Repeat([]byte("p"), 9*1024*1024)
	put := authenticatedRequest(t, http.MethodPut, ordinaryURL, bytes.NewReader(ordinaryContent))
	response, err := client.Do(put)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	ordinaryETag := response.Header.Get("ETag")
	_ = readResponse(t, response)

	response, err = getResponse(t, client, ordinaryURL+"?partNumber=1")
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Equal(t, strconv.Itoa(len(ordinaryContent)), response.Header.Get("Content-Length"))
	require.Equal(
		t,
		fmt.Sprintf("bytes 0-%d/%d", len(ordinaryContent)-1, len(ordinaryContent)),
		response.Header.Get("Content-Range"),
	)
	require.Empty(t, response.Header.Get("X-Amz-Mp-Parts-Count"))
	require.Equal(t, ordinaryContent, readResponse(t, response))

	request := authenticatedRequest(t, http.MethodHead, ordinaryURL+"?partNumber=1", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, ordinaryETag, response.Header.Get("ETag"))
	require.Equal(t, strconv.Itoa(len(ordinaryContent)), response.Header.Get("Content-Length"))
	_ = readResponse(t, response)

	response, err = getResponse(t, client, ordinaryURL+"?partNumber=2")
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, response.StatusCode)
	errorBody := string(readResponse(t, response))
	require.Contains(t, errorBody, "<Code>InvalidPartNumber</Code>")
	require.Contains(t, errorBody, "<PartNumberRequested>2</PartNumberRequested>")
	require.Contains(t, errorBody, "<ActualPartCount>1</ActualPartCount>")

	request = authenticatedRequest(t, http.MethodGet, ordinaryURL+"?partNumber=1", nil)
	request.Header.Set("Range", "bytes=0-1")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InvalidRequest")

	zeroURL := environment.server.URL + "/hackmd/zero.bin"
	response, err = client.Do(authenticatedRequest(t, http.MethodPut, zeroURL, bytes.NewReader(nil)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	response, err = getResponse(t, client, zeroURL+"?partNumber=1")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "0", response.Header.Get("Content-Length"))
	require.Empty(t, response.Header.Get("Content-Range"))
	require.Empty(t, readResponse(t, response))

	multipartURL := environment.server.URL + "/hackmd/final-multipart.bin"
	firstContent := bytes.Repeat([]byte("a"), 5*1024*1024)
	secondContent := bytes.Repeat([]byte("b"), 9*1024*1024)
	uploadID := createIntegrationMultipart(t, client, multipartURL)
	firstETag := uploadIntegrationPart(t, client, multipartURL, uploadID, 2, firstContent)
	secondETag := uploadIntegrationPart(t, client, multipartURL, uploadID, 9, secondContent)
	finalETag := completeReadIntegrationMultipart(
		t,
		client,
		multipartURL,
		uploadID,
		[]completedReadPart{{number: 2, etag: firstETag}, {number: 9, etag: secondETag}},
	)

	response, err = getResponse(t, client, multipartURL+"?partNumber=2")
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Equal(t, "2", response.Header.Get("X-Amz-Mp-Parts-Count"))
	require.Equal(t, strconv.Itoa(len(secondContent)), response.Header.Get("Content-Length"))
	require.Equal(
		t,
		fmt.Sprintf(
			"bytes %d-%d/%d",
			len(firstContent),
			len(firstContent)+len(secondContent)-1,
			len(firstContent)+len(secondContent),
		),
		response.Header.Get("Content-Range"),
	)
	require.Empty(t, response.Header.Get("X-Amz-Checksum-Crc64nvme"))
	require.Equal(t, secondContent, readResponse(t, response))

	request = authenticatedRequest(
		t,
		http.MethodGet,
		multipartURL+
			"?partNumber=2&response-content-disposition="+
			url.QueryEscape(`attachment; filename="part.bin"`),
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Empty(t, response.Header.Get("Content-Disposition"))
	require.Equal(t, secondContent, readResponse(t, response))

	request = authenticatedRequest(t, http.MethodHead, multipartURL+"?partNumber=2", nil)
	request.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(
		t,
		integrationChecksumValue(t, s3checksum.AlgorithmCRC64NVME, secondContent),
		response.Header.Get("X-Amz-Checksum-Crc64nvme"),
	)
	require.Equal(t, "FULL_OBJECT", response.Header.Get("X-Amz-Checksum-Type"))
	_ = readResponse(t, response)

	attributes := getIntegrationObjectAttributes(
		t,
		client,
		multipartURL,
		"ETag,Checksum,ObjectParts,StorageClass,ObjectSize",
		0,
		1,
	)
	require.Equal(t, finalETag, attributes.ETag)
	require.Equal(t, objectStorageClassStandardForTest, attributes.StorageClass)
	require.Equal(t, int64(len(firstContent)+len(secondContent)), attributes.ObjectSize)
	require.Equal(t, 2, attributes.ObjectParts.PartsCount)
	require.True(t, attributes.ObjectParts.IsTruncated)
	require.Equal(t, 1, attributes.ObjectParts.NextPartNumberMarker)
	require.Len(t, attributes.ObjectParts.Parts, 1)
	require.Equal(t, 1, attributes.ObjectParts.Parts[0].PartNumber)
	require.Equal(t, int64(len(firstContent)), attributes.ObjectParts.Parts[0].Size)
	require.Equal(
		t,
		integrationChecksumValue(t, s3checksum.AlgorithmCRC64NVME, firstContent),
		attributes.ObjectParts.Parts[0].ChecksumCRC64NVME,
	)

	secondPage := getIntegrationObjectAttributes(
		t,
		client,
		multipartURL,
		"ObjectParts",
		attributes.ObjectParts.NextPartNumberMarker,
		1,
	)
	require.False(t, secondPage.ObjectParts.IsTruncated)
	require.Len(t, secondPage.ObjectParts.Parts, 1)
	require.Equal(t, 2, secondPage.ObjectParts.Parts[0].PartNumber)
	require.Equal(t, int64(len(secondContent)), secondPage.ObjectParts.Parts[0].Size)

	ordinaryAttributes := getIntegrationObjectAttributes(
		t,
		client,
		ordinaryURL,
		"ETag,ObjectParts,ObjectSize",
		0,
		1000,
	)
	require.Equal(t, ordinaryETag, ordinaryAttributes.ETag)
	require.Nil(t, ordinaryAttributes.ObjectParts)

	require.Equal(t, 2, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab",
	))
	maxZero := getIntegrationObjectAttributes(
		t,
		client,
		multipartURL,
		"ObjectParts",
		0,
		0,
	)
	require.NotNil(t, maxZero.ObjectParts)
	require.False(t, maxZero.ObjectParts.IsTruncated)
	require.Empty(t, maxZero.ObjectParts.Parts)
	require.Zero(t, maxZero.ObjectParts.NextPartNumberMarker)

	endPage := getIntegrationObjectAttributes(
		t,
		client,
		multipartURL,
		"ObjectParts",
		10000,
		1000,
	)
	require.NotNil(t, endPage.ObjectParts)
	require.False(t, endPage.ObjectParts.IsTruncated)
	require.Empty(t, endPage.ObjectParts.Parts)

	publicAttributes := authenticatedRequest(
		t,
		http.MethodGet,
		multipartURL+"?attributes",
		nil,
	)
	publicAttributes.Header.Del("Authorization")
	publicAttributes.Header.Set("X-Amz-Object-Attributes", "ETag")
	response, err = client.Do(publicAttributes)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	privateURL := environment.server.URL + "/private-data/private-object.bin"
	response, err = client.Do(
		authenticatedRequest(t, http.MethodPut, privateURL, bytes.NewReader([]byte("private"))),
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	privateAttributes, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		privateURL+"?attributes",
		nil,
	)
	require.NoError(t, err)
	privateAttributes.Header.Set("X-Amz-Object-Attributes", "ETag")
	response, err = client.Do(privateAttributes)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_ = readResponse(t, response)
	privateAttributes = authenticatedRequest(
		t,
		http.MethodGet,
		privateURL+"?attributes",
		nil,
	)
	privateAttributes.Header.Set("X-Amz-Object-Attributes", "ETag")
	response, err = client.Do(privateAttributes)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	assertInvalidObjectAttributeRequests(t, client, multipartURL)
}

type completedReadPart struct {
	number int
	etag   string
}

func completeReadIntegrationMultipart(
	t *testing.T,
	client *http.Client,
	objectURL string,
	uploadID string,
	parts []completedReadPart,
) string {
	t.Helper()
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for _, part := range parts {
		_, _ = fmt.Fprintf(
			&body,
			"<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>",
			part.number,
			part.etag,
		)
	}
	body.WriteString("</CompleteMultipartUpload>")
	raw := []byte(body.String())
	request := authenticatedRequest(
		t,
		http.MethodPost,
		objectURL+"?uploadId="+uploadID,
		bytes.NewReader(raw),
	)
	digest := filemgr.NewMD5CompatibilityHash()
	_, err := digest.Write(raw)
	require.NoError(t, err)
	request.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(digest.Sum(nil)))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	return response.Header.Get("ETag")
}

const objectStorageClassStandardForTest = "STANDARD"

type integrationObjectAttributes struct {
	ETag         string                              `xml:"ETag"`
	Checksum     integrationObjectAttributesChecksum `xml:"Checksum"`
	ObjectParts  *integrationObjectParts             `xml:"ObjectParts"`
	StorageClass string                              `xml:"StorageClass"`
	ObjectSize   int64                               `xml:"ObjectSize"`
}

type integrationObjectAttributesChecksum struct {
	CRC64NVME string `xml:"ChecksumCRC64NVME"`
	Type      string `xml:"ChecksumType"`
}

type integrationObjectParts struct {
	IsTruncated          bool                    `xml:"IsTruncated"`
	MaxParts             int                     `xml:"MaxParts"`
	NextPartNumberMarker int                     `xml:"NextPartNumberMarker"`
	PartNumberMarker     int                     `xml:"PartNumberMarker"`
	Parts                []integrationObjectPart `xml:"Part"`
	PartsCount           int                     `xml:"PartsCount"`
}

type integrationObjectPart struct {
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME"`
	PartNumber        int    `xml:"PartNumber"`
	Size              int64  `xml:"Size"`
}

func getIntegrationObjectAttributes(
	t *testing.T,
	client *http.Client,
	objectURL string,
	attributes string,
	marker int,
	maxParts int,
) integrationObjectAttributes {
	t.Helper()
	request := authenticatedRequest(t, http.MethodGet, objectURL+"?attributes", nil)
	request.Header.Set("X-Amz-Object-Attributes", attributes)
	request.Header.Set("X-Amz-Part-Number-Marker", strconv.Itoa(marker))
	request.Header.Set("X-Amz-Max-Parts", strconv.Itoa(maxParts))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "application/xml; charset=utf-8", response.Header.Get("Content-Type"))
	require.NotEmpty(t, response.Header.Get("Last-Modified"))
	var result integrationObjectAttributes
	require.NoError(t, xml.Unmarshal(readResponse(t, response), &result))
	return result
}

func assertInvalidObjectAttributeRequests(t *testing.T, client *http.Client, objectURL string) {
	t.Helper()
	tests := []struct {
		target     string
		attributes string
		headers    map[string]string
		status     int
		code       string
	}{
		{target: objectURL + "?attributes", status: 400, code: "InvalidRequest"},
		{target: objectURL + "?attributes=value", attributes: "ETag", status: 400, code: "InvalidRequest"},
		{target: objectURL + "?attributes", attributes: "", status: 400, code: "InvalidRequest"},
		{target: objectURL + "?attributes", attributes: "ETag,ETag", status: 400, code: "InvalidArgument"},
		{target: objectURL + "?attributes", attributes: "Unknown", status: 400, code: "InvalidArgument"},
		{
			target:     objectURL + "?attributes",
			attributes: "ObjectParts",
			headers:    map[string]string{"X-Amz-Max-Parts": "1001"},
			status:     400,
			code:       "InvalidArgument",
		},
		{
			target:     objectURL + "?attributes",
			attributes: "ObjectParts",
			headers:    map[string]string{"X-Amz-Part-Number-Marker": "-1"},
			status:     400,
			code:       "InvalidArgument",
		},
		{
			target:     objectURL + "?attributes&partNumber=1",
			attributes: "ETag",
			status:     400,
			code:       "InvalidRequest",
		},
		{
			target:     objectURL + "?attributes&versionId=1",
			attributes: "ETag",
			status:     501,
			code:       "NotImplemented",
		},
	}
	for _, test := range tests {
		request := authenticatedRequest(t, http.MethodGet, test.target, nil)
		if test.attributes != "" {
			request.Header.Set("X-Amz-Object-Attributes", test.attributes)
		}
		for name, value := range test.headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		require.NoError(t, err)
		require.Equal(t, test.status, response.StatusCode, test.target)
		require.Contains(t, string(readResponse(t, response)), "<Code>"+test.code+"</Code>")
	}
}

func TestS3ReadQueryPresigningIncludesOverrides(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	content := []byte("signed response")
	objectURL := environment.server.URL + "/private-data/signed-response.bin"
	payloadDigest := sha256.Sum256(content)
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		objectURL,
		bytes.NewReader(content),
	)
	require.NoError(t, err)
	request.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(payloadDigest[:]))
	signer := awsv4.NewSigner()
	require.NoError(t, signer.SignHTTP(
		t.Context(),
		aws.Credentials{AccessKeyID: "access", SecretAccessKey: "secret"},
		request,
		hex.EncodeToString(payloadDigest[:]),
		"s3",
		"us-east-1",
		time.Now(),
	))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	assertPresignedResponseOverride(t, client, objectURL, content)
}
