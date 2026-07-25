package s3

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
)

func TestDecodeMultipartCompleteBodyIsStrict(t *testing.T) {
	valid := `<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Part><PartNumber>1</PartNumber><ETag>"0123456789ABCDEF0123456789ABCDEF"</ETag></Part>` +
		`<Part><PartNumber>3</PartNumber><ETag>abcdefabcdefabcdefabcdefabcdefab</ETag></Part>` +
		`</CompleteMultipartUpload>`
	parts, apiError := decodeMultipartCompleteBody([]byte(valid))
	require.Nil(t, apiError)
	require.Len(t, parts, 2)
	require.Equal(t, 1, parts[0].PartNumber)
	require.Equal(t, "0123456789abcdef0123456789abcdef", parts[0].ETag)
	require.Equal(t, 3, parts[1].PartNumber)

	checksummed := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber>` +
		`<ETag>0123456789abcdef0123456789abcdef</ETag>` +
		`<ChecksumSHA256>I59Z7VXnN8dxR89VrQwbAwttfudIp0JpUvm4UtWpNeU=</ChecksumSHA256>` +
		`</Part></CompleteMultipartUpload>`
	parts, apiError = decodeMultipartCompleteBody([]byte(checksummed))
	require.Nil(t, apiError)
	require.Equal(t, s3checksum.AlgorithmSHA256, parts[0].ChecksumAlgorithm)
	require.Equal(t, "I59Z7VXnN8dxR89VrQwbAwttfudIp0JpUvm4UtWpNeU=", parts[0].ChecksumValue)

	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty", body: "", code: "MalformedXML"},
		{
			name: "multiple checksums",
			body: `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber>` +
				`<ETag>0123456789abcdef0123456789abcdef</ETag>` +
				`<ChecksumCRC32>AAAAAA==</ChecksumCRC32>` +
				`<ChecksumCRC32C>AAAAAA==</ChecksumCRC32C>` +
				`</Part></CompleteMultipartUpload>`,
			code: "MalformedXML",
		},
		{
			name: "unknown element",
			body: `<CompleteMultipartUpload><Unknown/></CompleteMultipartUpload>`,
			code: "MalformedXML",
		},
		{
			name: "duplicate number",
			body: `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber>` +
				`<PartNumber>1</PartNumber><ETag>0123456789abcdef0123456789abcdef</ETag>` +
				`</Part></CompleteMultipartUpload>`,
			code: "MalformedXML",
		},
		{
			name: "DTD",
			body: `<!DOCTYPE CompleteMultipartUpload [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>` +
				`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber>` +
				`<ETag>&xxe;</ETag></Part></CompleteMultipartUpload>`,
			code: "MalformedXML",
		},
		{
			name: "invalid part order",
			body: `<CompleteMultipartUpload>` +
				`<Part><PartNumber>2</PartNumber><ETag>0123456789abcdef0123456789abcdef</ETag></Part>` +
				`<Part><PartNumber>1</PartNumber><ETag>abcdefabcdefabcdefabcdefabcdefab</ETag></Part>` +
				`</CompleteMultipartUpload>`,
			code: "InvalidPartOrder",
		},
		{
			name: "weak ETag",
			body: `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber>` +
				`<ETag>W/"0123456789abcdef0123456789abcdef"</ETag>` +
				`</Part></CompleteMultipartUpload>`,
			code: "InvalidPart",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, apiError := decodeMultipartCompleteBody([]byte(test.body))
			require.NotNil(t, apiError)
			require.Equal(t, test.code, apiError.Code)
		})
	}
}

func TestCreateMultipartChecksumDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name          string
		algorithm     string
		checksumType  string
		wantAlgorithm s3checksum.Algorithm
		wantType      s3checksum.Type
		wantCode      string
	}{
		{
			name:          "default",
			wantAlgorithm: s3checksum.AlgorithmCRC64NVME,
			wantType:      s3checksum.TypeFullObject,
		},
		{
			name:          "sha256 default composite",
			algorithm:     "SHA256",
			wantAlgorithm: s3checksum.AlgorithmSHA256,
			wantType:      s3checksum.TypeComposite,
		},
		{
			name:          "crc32 full object",
			algorithm:     "CRC32",
			checksumType:  "FULL_OBJECT",
			wantAlgorithm: s3checksum.AlgorithmCRC32,
			wantType:      s3checksum.TypeFullObject,
		},
		{name: "composite without algorithm", checksumType: "COMPOSITE", wantCode: "InvalidRequest"},
		{name: "unsupported", algorithm: "SHA512", wantCode: "NotImplemented"},
		{name: "unknown", algorithm: "crc32", wantCode: "InvalidArgument"},
		{
			name:         "sha full object",
			algorithm:    "SHA1",
			checksumType: "FULL_OBJECT",
			wantCode:     "InvalidRequest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/bucket/key?uploads",
				nil,
			)
			if test.algorithm != "" {
				request.Header.Set("X-Amz-Checksum-Algorithm", test.algorithm)
			}
			if test.checksumType != "" {
				request.Header.Set("X-Amz-Checksum-Type", test.checksumType)
			}
			algorithm, checksumType, apiError := parseCreateMultipartChecksum(request)
			if test.wantCode != "" {
				require.NotNil(t, apiError)
				require.Equal(t, test.wantCode, apiError.Code)
				return
			}
			require.Nil(t, apiError)
			require.Equal(t, test.wantAlgorithm, algorithm)
			require.Equal(t, test.wantType, checksumType)
		})
	}
}

func TestCreateMultipartChecksumRejectsConflictingSingletonHeaders(t *testing.T) {
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/bucket/key?uploads",
		nil,
	)
	request.Header.Add("X-Amz-Checksum-Algorithm", "CRC32")
	request.Header.Add("X-Amz-Checksum-Algorithm", "CRC32C")

	_, _, apiError := parseCreateMultipartChecksum(request)

	require.NotNil(t, apiError)
	require.Equal(t, "InvalidRequest", apiError.Code)
}

func TestMultipartUploadChecksumPolicy(t *testing.T) {
	content := []byte("payload")
	checksumHash, err := s3checksum.NewHash(s3checksum.AlgorithmCRC32C)
	require.NoError(t, err)
	_, err = checksumHash.Write(content)
	require.NoError(t, err)
	checksumValue := s3checksum.SumBase64(checksumHash)

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/bucket/key",
		bytes.NewReader(content),
	)
	request.Header.Set("X-Amz-Checksum-Crc32c", checksumValue)
	request.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC32C")
	hashes, reader, apiError := newMultipartUploadHashes(request, &filemgr.MultipartChecksumSpec{
		Algorithm:    s3checksum.AlgorithmCRC32C,
		ChecksumType: s3checksum.TypeComposite,
	})
	require.Nil(t, apiError)
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.Nil(t, hashes.validate())
	require.Equal(t, checksumValue, hashes.checksumValue())

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/bucket/key",
		bytes.NewReader(content),
	)
	_, _, apiError = newMultipartUploadHashes(request, &filemgr.MultipartChecksumSpec{
		Algorithm:    s3checksum.AlgorithmCRC32C,
		ChecksumType: s3checksum.TypeComposite,
	})
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidPart", apiError.Code)

	request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/bucket/key",
		bytes.NewReader(content),
	)
	hashes, reader, apiError = newMultipartUploadHashes(request, &filemgr.MultipartChecksumSpec{
		Algorithm:    s3checksum.AlgorithmCRC64NVME,
		ChecksumType: s3checksum.TypeFullObject,
	})
	require.Nil(t, apiError)
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.Nil(t, hashes.validate())
	require.NotEmpty(t, hashes.checksumValue())
}

func TestCompleteMultipartChecksumHeaders(t *testing.T) {
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/bucket/key?uploadId=id",
		nil,
	)
	request.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	request.Header.Set("X-Amz-Checksum-Type", "COMPOSITE")
	request.Header.Set("X-Amz-Mp-Object-Size", "123")
	headers, apiError := parseCompleteChecksumHeaders(request)
	require.Nil(t, apiError)
	require.Equal(t, s3checksum.AlgorithmSHA256, headers.algorithm)
	require.Equal(t, s3checksum.TypeComposite, headers.checksumType)
	require.Equal(t, int64(123), *headers.expectedSize)

	request.Header.Set("X-Amz-Mp-Object-Size", "-1")
	_, apiError = parseCompleteChecksumHeaders(request)
	require.NotNil(t, apiError)
	require.Equal(t, "InvalidRequest", apiError.Code)
}

func TestValidateMultipartQueryRejectsAmbiguousParameters(t *testing.T) {
	require.Nil(t, validateMultipartQuery(
		url.Values{
			"uploadId": {"id"},
			"x-id":     {"ListParts"},
		},
		[]string{"uploadId"},
		nil,
	))
	for _, query := range []url.Values{
		{"uploadId": {"first", "second"}},
		{"uploadId": {"id"}, "unknown": {"value"}},
		{"UploadId": {"id"}},
		{"uploadId": {"id"}, "x-id": {"one", "two"}},
	} {
		apiError := validateMultipartQuery(query, []string{"uploadId"}, nil)
		require.NotNil(t, apiError, strings.Join(query["uploadId"], ","))
		require.Equal(t, "InvalidArgument", apiError.Code)
	}
}
