package s3

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/trace"
	"github.com/xxxsen/s3verify"
)

type BucketACL string

var (
	errAuthenticationRequired = errors.New("authentication is required")
	errBasicAuthentication    = errors.New("basic authentication failed")
)

const (
	BucketACLPrivate    BucketACL = "private"
	BucketACLPublicRead BucketACL = "public-read"
)

type Bucket struct {
	Name string
	ACL  BucketACL
}

type Config struct {
	Buckets              []Bucket
	MaxObjectSize        int64
	MultipartExpireHours int
	Users                map[string]string
}

type S3Handler struct {
	fmgr            filemgr.IFileManager
	locks           *pathLocker
	multipartLocks  *pathLocker
	buckets         map[string]Bucket
	bucketList      []Bucket
	maxObjectSize   int64
	multipartExpiry time.Duration
	users           map[string]string
	verifier        *s3verify.Verifier
}

func NewS3Handler(fmgr filemgr.IFileManager, configs ...Config) *S3Handler {
	config := Config{
		Buckets: []Bucket{{Name: "hackmd", ACL: BucketACLPublicRead}},
		Users:   map[string]string{},
	}
	if len(configs) != 0 {
		config = configs[0]
	}
	if config.MultipartExpireHours == 0 {
		config.MultipartExpireHours = 24
	}
	users := make(map[string]string, len(config.Users))
	for accessKey, secret := range config.Users {
		users[accessKey] = secret
	}
	provider := s3verify.CredentialProviderFunc(func(
		_ context.Context,
		accessKey string,
	) (s3verify.Credential, bool, error) {
		secret, exists := users[accessKey]
		if !exists {
			return s3verify.Credential{}, false, nil
		}
		return s3verify.Credential{
			AccessKeyID:     accessKey,
			SecretAccessKey: secret,
		}, true, nil
	})
	verifier, err := s3verify.New(provider, s3verify.Options{Region: "us-east-1", Service: "s3"})
	if err != nil {
		panic(fmt.Errorf("create S3 signature verifier: %w", err))
	}
	buckets := make(map[string]Bucket, len(config.Buckets))
	bucketList := append([]Bucket(nil), config.Buckets...)
	for _, bucket := range bucketList {
		buckets[bucket.Name] = bucket
	}
	return &S3Handler{
		fmgr:            fmgr,
		locks:           newPathLocker(),
		multipartLocks:  newPathLocker(),
		buckets:         buckets,
		bucketList:      bucketList,
		maxObjectSize:   config.MaxObjectSize,
		multipartExpiry: time.Duration(config.MultipartExpireHours) * time.Hour,
		users:           users,
		verifier:        verifier,
	}
}

func (h *S3Handler) Bucket(name string) (Bucket, bool) {
	bucket, exists := h.buckets[name]
	return bucket, exists
}

func (h *S3Handler) RequestID(c *gin.Context) {
	requestID, _ := trace.GetTraceId(c.Request.Context())
	c.Header("x-amz-request-id", requestID)
	c.Next()
}

func (h *S3Handler) Authorize(c *gin.Context, required bool) (*s3verify.Result, *s3base.APIError) {
	authorizationValues := c.Request.Header.Values("Authorization")
	hasAuthorization := len(authorizationValues) != 0
	hasSigQuery := requestHasSignatureQuery(c.Request)
	if !hasAuthorization && !hasSigQuery {
		if required {
			return nil, s3base.AccessDenied(errAuthenticationRequired)
		}
		return nil, nil
	}
	if len(authorizationValues) > 1 {
		return nil, s3base.InvalidRequest("Multiple authentication mechanisms are not allowed.", nil)
	}
	if hasAuthorization && strings.HasPrefix(authorizationValues[0], "Basic ") {
		if hasSigQuery {
			return nil, s3base.InvalidRequest("Multiple authentication mechanisms are not allowed.", nil)
		}
		accessKey, secret, ok := c.Request.BasicAuth()
		expected, exists := h.users[accessKey]
		if !ok || !exists || !hmac.Equal([]byte(expected), []byte(secret)) {
			return nil, s3base.AccessDenied(errBasicAuthentication)
		}
		return nil, nil
	}
	result, err := h.verifier.Verify(c.Request.Context(), c.Request)
	if err != nil {
		return nil, verifierAPIError(err)
	}
	c.Request.Body = result.Body
	if result.DecodedContentLength >= 0 {
		c.Set("s3-decoded-content-length", result.DecodedContentLength)
	}
	c.Set("s3-verified-trailers", result.Trailers)
	return result, nil
}

func requestHasSignatureQuery(request *http.Request) bool {
	for key := range request.URL.Query() {
		switch strings.ToLower(key) {
		case "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
			"x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
			return true
		}
	}
	return false
}

func verifierAPIError(err error) *s3base.APIError {
	var verifyError *s3verify.VerifyError
	if !errors.As(err, &verifyError) {
		return s3base.InternalError(err)
	}
	switch verifyError.Code {
	case s3verify.ErrorSignatureMismatch:
		return s3base.NewError(http.StatusForbidden, "SignatureDoesNotMatch", "The request signature is invalid.", err)
	case s3verify.ErrorRequestTimeSkewed:
		return s3base.NewError(
			http.StatusForbidden,
			"RequestTimeTooSkewed",
			"The difference between the request time and server time is too large.",
			err,
		)
	case s3verify.ErrorUnknownCredential, s3verify.ErrorSessionTokenMismatch,
		s3verify.ErrorMissingAuthentication, s3verify.ErrorPresignedExpired,
		s3verify.ErrorPresignedNotYetValid:
		return s3base.AccessDenied(err)
	case s3verify.ErrorMalformedQuery:
		return s3base.NewError(
			http.StatusBadRequest,
			"AuthorizationQueryParametersError",
			"The authorization query parameters are malformed.",
			err,
		)
	case s3verify.ErrorPayloadHashMismatch, s3verify.ErrorChunkSignatureMismatch,
		s3verify.ErrorTrailerSignatureMismatch:
		return s3base.NewError(
			http.StatusBadRequest,
			"XAmzContentSHA256Mismatch",
			"The provided x-amz-content-sha256 does not match the payload.",
			err,
		)
	case s3verify.ErrorCredentialProvider:
		return s3base.InternalError(err)
	case s3verify.ErrorAmbiguousAuthentication, s3verify.ErrorMalformedAuthorization,
		s3verify.ErrorMalformedCredential, s3verify.ErrorMalformedDate,
		s3verify.ErrorMalformedSignedHeaders, s3verify.ErrorUnsupportedAlgorithm,
		s3verify.ErrorUnsupportedPayloadMode, s3verify.ErrorMalformedChunk,
		s3verify.ErrorDecodedLengthMismatch, s3verify.ErrorMalformedTrailer:
		return s3base.NewError(
			http.StatusBadRequest,
			"AuthorizationHeaderMalformed",
			"The authorization header is malformed.",
			err,
		)
	default:
		return s3base.InternalError(err)
	}
}

type pathLockEntry struct {
	mutex sync.Mutex
	refs  int
}

type pathLocker struct {
	mutex sync.Mutex
	locks map[string]*pathLockEntry
}

func newPathLocker() *pathLocker {
	return &pathLocker{locks: make(map[string]*pathLockEntry)}
}

func (l *pathLocker) lock(key string) func() {
	l.mutex.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &pathLockEntry{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mutex.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		l.mutex.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mutex.Unlock()
	}
}
