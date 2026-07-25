// Package s3checksum implements the checksum algorithms and multipart
// aggregation rules used by the tgfile S3 compatibility layer.
package s3checksum

import (
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"
)

// Algorithm is an S3 checksum algorithm name.
type Algorithm string

const (
	// AlgorithmCRC32 is the IEEE CRC-32 algorithm.
	AlgorithmCRC32 Algorithm = "CRC32"
	// AlgorithmCRC32C is the Castagnoli CRC-32C algorithm.
	AlgorithmCRC32C Algorithm = "CRC32C"
	// AlgorithmCRC64NVME is the CRC-64/NVME algorithm.
	AlgorithmCRC64NVME Algorithm = "CRC64NVME"
	// AlgorithmSHA1 is the SHA-1 compatibility checksum algorithm.
	AlgorithmSHA1 Algorithm = "SHA1"
	// AlgorithmSHA256 is the SHA-256 checksum algorithm.
	AlgorithmSHA256 Algorithm = "SHA256"
)

// Type is the S3 multipart checksum aggregation type.
type Type string

const (
	// TypeFullObject represents a checksum over the complete object bytes.
	TypeFullObject Type = "FULL_OBJECT"
	// TypeComposite represents a checksum over ordered part checksums.
	TypeComposite Type = "COMPOSITE"
)

var (
	// ErrInvalidAlgorithm indicates an algorithm name outside the S3 checksum vocabulary.
	ErrInvalidAlgorithm = errors.New("invalid S3 checksum algorithm")
	// ErrUnsupportedAlgorithm indicates an S3 algorithm not implemented by tgfile.
	ErrUnsupportedAlgorithm = errors.New("unsupported S3 checksum algorithm")
	// ErrInvalidType indicates an unknown checksum aggregation type.
	ErrInvalidType = errors.New("invalid S3 checksum type")
	// ErrInvalidCombination indicates an unsupported algorithm/type pair.
	ErrInvalidCombination = errors.New("invalid S3 checksum algorithm and type combination")
)

var unsupportedAlgorithms = map[string]struct{}{
	"MD5":       {},
	"SHA512":    {},
	"XXHASH64":  {},
	"XXHASH3":   {},
	"XXHASH128": {},
}

const crc64NVMEPolynomial = uint64(0x9a6c9329ac4bc9b5)

// ResolveMultipart applies tgfile's deterministic CreateMultipartUpload defaults
// and validates the resulting algorithm/type pair.
func ResolveMultipart(algorithmValue, typeValue string) (Algorithm, Type, error) {
	algorithm, err := parseAlgorithmWithDefault(algorithmValue, typeValue)
	if err != nil {
		return "", "", err
	}
	checksumType, err := parseTypeWithDefault(algorithm, typeValue)
	if err != nil {
		return "", "", err
	}
	if err := ValidateCombination(algorithm, checksumType); err != nil {
		return "", "", err
	}
	return algorithm, checksumType, nil
}

func parseAlgorithmWithDefault(algorithmValue, typeValue string) (Algorithm, error) {
	if algorithmValue == "" {
		if typeValue == string(TypeComposite) {
			return "", fmt.Errorf("%w: a composite type requires an explicit algorithm", ErrInvalidCombination)
		}
		return AlgorithmCRC64NVME, nil
	}
	if _, exists := unsupportedAlgorithms[algorithmValue]; exists {
		return "", fmt.Errorf("%w: %s", ErrUnsupportedAlgorithm, algorithmValue)
	}
	switch Algorithm(algorithmValue) {
	case AlgorithmCRC32, AlgorithmCRC32C, AlgorithmCRC64NVME, AlgorithmSHA1, AlgorithmSHA256:
		return Algorithm(algorithmValue), nil
	default:
		return "", fmt.Errorf("%w: %s", ErrInvalidAlgorithm, algorithmValue)
	}
}

func parseTypeWithDefault(algorithm Algorithm, typeValue string) (Type, error) {
	if typeValue == "" {
		if algorithm == AlgorithmCRC64NVME {
			return TypeFullObject, nil
		}
		return TypeComposite, nil
	}
	switch Type(typeValue) {
	case TypeFullObject, TypeComposite:
		return Type(typeValue), nil
	default:
		return "", fmt.Errorf("%w: %s", ErrInvalidType, typeValue)
	}
}

// ParseAlgorithm validates a non-empty, explicitly supplied algorithm.
func ParseAlgorithm(value string) (Algorithm, error) {
	if value == "" {
		return "", fmt.Errorf("%w: empty value", ErrInvalidAlgorithm)
	}
	return parseAlgorithmWithDefault(value, "")
}

// ParseType validates a non-empty checksum aggregation type.
func ParseType(value string) (Type, error) {
	if value == "" {
		return "", fmt.Errorf("%w: empty value", ErrInvalidType)
	}
	return parseTypeWithDefault(AlgorithmCRC64NVME, value)
}

// ValidateCombination verifies that an algorithm supports the requested
// multipart aggregation type.
func ValidateCombination(algorithm Algorithm, checksumType Type) error {
	switch checksumType {
	case TypeFullObject:
		switch algorithm {
		case AlgorithmCRC32, AlgorithmCRC32C, AlgorithmCRC64NVME:
			return nil
		case AlgorithmSHA1, AlgorithmSHA256:
			return fmt.Errorf("%w: %s cannot use %s", ErrInvalidCombination, algorithm, checksumType)
		default:
			return fmt.Errorf("%w: %s", ErrInvalidAlgorithm, algorithm)
		}
	case TypeComposite:
		switch algorithm {
		case AlgorithmCRC32, AlgorithmCRC32C, AlgorithmSHA1, AlgorithmSHA256:
			return nil
		case AlgorithmCRC64NVME:
			return fmt.Errorf("%w: %s cannot use %s", ErrInvalidCombination, algorithm, checksumType)
		default:
			return fmt.Errorf("%w: %s", ErrInvalidAlgorithm, algorithm)
		}
	default:
		return fmt.Errorf("%w: %s", ErrInvalidType, checksumType)
	}
}

// NewHash constructs the exact hash implementation used on the S3 wire.
func NewHash(algorithm Algorithm) (hash.Hash, error) {
	switch algorithm {
	case AlgorithmCRC32:
		return crc32.NewIEEE(), nil
	case AlgorithmCRC32C:
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case AlgorithmCRC64NVME:
		return crc64.New(crc64.MakeTable(crc64NVMEPolynomial)), nil
	case AlgorithmSHA1:
		return newSHA1Hash(), nil
	case AlgorithmSHA256:
		return newSHA256Hash(), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidAlgorithm, algorithm)
	}
}

// HeaderName returns the algorithm-specific S3 checksum header.
func HeaderName(algorithm Algorithm) (string, error) {
	if _, err := NewHash(algorithm); err != nil {
		return "", err
	}
	return "x-amz-checksum-" + strings.ToLower(string(algorithm)), nil
}

// XMLName returns the algorithm-specific S3 checksum XML element.
func XMLName(algorithm Algorithm) (string, error) {
	if _, err := NewHash(algorithm); err != nil {
		return "", err
	}
	return "Checksum" + string(algorithm), nil
}
