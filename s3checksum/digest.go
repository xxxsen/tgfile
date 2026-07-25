package s3checksum

import (
	"crypto/sha1" //nolint:gosec // SHA-1 is required by the S3 checksum wire protocol, not for security.
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
)

var (
	// ErrInvalidDigest indicates malformed Base64 or an incorrect digest size.
	ErrInvalidDigest = errors.New("invalid S3 checksum digest")
	// ErrInvalidPartSize indicates an impossible multipart part size.
	ErrInvalidPartSize = errors.New("invalid S3 multipart part size")
)

func newSHA1Hash() hash.Hash {
	return sha1.New() //nolint:gosec // SHA-1 is required by the S3 checksum wire protocol, not for security.
}

func newSHA256Hash() hash.Hash {
	return sha256.New()
}

// Decode validates and decodes an algorithm-specific Base64 digest.
func Decode(algorithm Algorithm, value string) ([]byte, error) {
	digest, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrInvalidDigest, algorithm, err)
	}
	hasher, err := NewHash(algorithm)
	if err != nil {
		return nil, err
	}
	if len(digest) != hasher.Size() {
		return nil, fmt.Errorf(
			"%w: %s digest has %d bytes, expected %d",
			ErrInvalidDigest,
			algorithm,
			len(digest),
			hasher.Size(),
		)
	}
	return digest, nil
}

// SumBase64 returns the Base64-encoded digest accumulated by hasher.
func SumBase64(hasher hash.Hash) string {
	return base64.StdEncoding.EncodeToString(hasher.Sum(nil))
}
