package s3

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty", body: "", code: "MalformedXML"},
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
