package s3

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/xxxsen/s3verify"

	"github.com/xxxsen/tgfile/s3checksum"
)

func TestDecodeDeleteObjectsAcceptsStandardNamespace(t *testing.T) {
	request, apiError := decodeDeleteObjects([]byte(
		`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Object><Key>one</Key><ETag>"etag"</ETag></Object><Quiet>true</Quiet></Delete>`,
	))

	require.Nil(t, apiError)
	require.Len(t, request.Objects, 1)
	require.Equal(t, "one", request.Objects[0].Key)
	require.True(t, request.Quiet)
}

func TestDecodeDeleteObjectsRejectsAmbiguousStructure(t *testing.T) {
	testCases := map[string]string{
		"multiple roots": `<Delete><Object><Key>one</Key></Object></Delete>` +
			`<Delete><Object><Key>two</Key></Object></Delete>`,
		"duplicate key": `<Delete><Object><Key>one</Key><Key>two</Key></Object></Delete>`,
		"missing key":   `<Delete><Object><ETag>"etag"</ETag></Object></Delete>`,
		"duplicate quiet": `<Delete><Object><Key>one</Key></Object>` +
			`<Quiet>true</Quiet><Quiet>false</Quiet></Delete>`,
		"mixed data": `<Delete>unexpected<Object><Key>one</Key></Object></Delete>`,
	}

	for name, body := range testCases {
		t.Run(name, func(t *testing.T) {
			_, apiError := decodeDeleteObjects([]byte(body))
			require.NotNil(t, apiError)
			require.Equal(t, "MalformedXML", apiError.Code)
		})
	}
}

type failingDeleteReader struct {
	err error
}

func (r *failingDeleteReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestReadDeleteBodyPreservesPayloadVerificationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/bucket?delete", nil)
	request.Body = io.NopCloser(&failingDeleteReader{
		err: &s3verify.VerifyError{Code: s3verify.ErrorPayloadHashMismatch},
	})
	request.Header.Set("Content-MD5", "1B2M2Y8AsgTpgAmY7PhCfg==")
	context.Request = request

	_, apiError := readDeleteBody(context)

	require.NotNil(t, apiError)
	require.Equal(t, "XAmzContentSHA256Mismatch", apiError.Code)
}

func TestReadDeleteBodyAcceptsAdditionalChecksum(t *testing.T) {
	body := []byte(`<Delete><Object><Key>object</Key></Object></Delete>`)
	digest, err := s3checksum.NewHash(s3checksum.AlgorithmCRC32)
	require.NoError(t, err)
	_, err = digest.Write(body)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/bucket?delete",
		bytes.NewReader(body),
	)
	request.Header.Set("X-Amz-Checksum-Crc32", s3checksum.SumBase64(digest))
	request.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC32")
	context.Request = request

	actual, apiError := readDeleteBody(context)

	require.Nil(t, apiError)
	require.Equal(t, body, actual)
}
