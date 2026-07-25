package s3checksum

import (
	"fmt"
	"strconv"
	"strings"
)

// Composite calculates the request checksum and the persisted S3 response
// value for ordered part checksums.
func Composite(algorithm Algorithm, values []string) (string, string, error) {
	if len(values) == 0 {
		return "", "", fmt.Errorf("%w: no parts", ErrInvalidDigest)
	}
	if err := ValidateCombination(algorithm, TypeComposite); err != nil {
		return "", "", err
	}
	hasher, err := NewHash(algorithm)
	if err != nil {
		return "", "", err
	}
	for _, value := range values {
		raw, err := Decode(algorithm, value)
		if err != nil {
			return "", "", err
		}
		if _, err := hasher.Write(raw); err != nil {
			return "", "", fmt.Errorf("write %s composite checksum: %w", algorithm, err)
		}
	}
	requestValue := SumBase64(hasher)
	return requestValue, requestValue + "-" + strconv.Itoa(len(values)), nil
}

// ParseCompositeStored validates a persisted response value and returns the
// Base64 request value without its part-count suffix.
func ParseCompositeStored(algorithm Algorithm, value string, expectedParts int) (string, error) {
	separator := strings.LastIndexByte(value, '-')
	if separator <= 0 || separator == len(value)-1 {
		return "", fmt.Errorf("%w: malformed composite suffix", ErrInvalidDigest)
	}
	partCount, err := strconv.Atoi(value[separator+1:])
	if err != nil || partCount != expectedParts || partCount <= 0 {
		return "", fmt.Errorf("%w: unexpected composite part count", ErrInvalidDigest)
	}
	requestValue := value[:separator]
	if _, err := Decode(algorithm, requestValue); err != nil {
		return "", err
	}
	return requestValue, nil
}
