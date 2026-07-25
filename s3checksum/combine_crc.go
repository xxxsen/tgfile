package s3checksum

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// PartChecksum is one ordered part checksum and its byte length.
type PartChecksum struct {
	Value string
	Size  int64
}

type crcWord interface {
	~uint32 | ~uint64
}

// FullObject combines ordered part CRCs into the checksum of their byte
// concatenation without reading the original object data.
func FullObject(algorithm Algorithm, parts []PartChecksum) (string, error) {
	if len(parts) == 0 {
		return "", fmt.Errorf("%w: no parts", ErrInvalidDigest)
	}
	if err := ValidateCombination(algorithm, TypeFullObject); err != nil {
		return "", err
	}
	switch algorithm {
	case AlgorithmCRC32, AlgorithmCRC32C:
		return combineCRC32Parts(algorithm, parts)
	case AlgorithmCRC64NVME:
		return combineCRC64Parts(parts)
	case AlgorithmSHA1, AlgorithmSHA256:
		return "", fmt.Errorf("%w: %s", ErrInvalidCombination, algorithm)
	default:
		return "", fmt.Errorf("%w: %s", ErrInvalidAlgorithm, algorithm)
	}
}

func combineCRC32Parts(algorithm Algorithm, parts []PartChecksum) (string, error) {
	polynomial := crc32IEEEPolynomial
	if algorithm == AlgorithmCRC32C {
		polynomial = crc32CastagnoliPolynomial
	}
	var combined uint32
	for index, part := range parts {
		if part.Size < 0 {
			return "", fmt.Errorf("%w: part %d", ErrInvalidPartSize, index+1)
		}
		raw, err := Decode(algorithm, part.Value)
		if err != nil {
			return "", err
		}
		partCRC := binary.BigEndian.Uint32(raw)
		if index == 0 {
			combined = partCRC
			continue
		}
		combined = combineCRC(combined, partCRC, part.Size, polynomial, 32)
	}
	raw := make([]byte, 4)
	binary.BigEndian.PutUint32(raw, combined)
	return base64.StdEncoding.EncodeToString(raw), nil
}

func combineCRC64Parts(parts []PartChecksum) (string, error) {
	var combined uint64
	for index, part := range parts {
		if part.Size < 0 {
			return "", fmt.Errorf("%w: part %d", ErrInvalidPartSize, index+1)
		}
		raw, err := Decode(AlgorithmCRC64NVME, part.Value)
		if err != nil {
			return "", err
		}
		partCRC := binary.BigEndian.Uint64(raw)
		if index == 0 {
			combined = partCRC
			continue
		}
		combined = combineCRC(combined, partCRC, part.Size, crc64NVMEPolynomial, 64)
	}
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, combined)
	return base64.StdEncoding.EncodeToString(raw), nil
}

const (
	crc32IEEEPolynomial       = uint32(0xedb88320)
	crc32CastagnoliPolynomial = uint32(0x82f63b78)
)

// combineCRC applies the reflected CRC zero-byte operator from zlib's
// crc32_combine algorithm to already finalized CRC values.
func combineCRC[T crcWord](first, second T, secondLength int64, polynomial T, width int) T {
	if secondLength <= 0 {
		return first
	}
	odd := make([]T, width)
	even := make([]T, width)
	odd[0] = polynomial
	row := T(1)
	for index := 1; index < width; index++ {
		odd[index] = row
		row <<= 1
	}
	gf2MatrixSquare(even, odd)
	gf2MatrixSquare(odd, even)
	for {
		gf2MatrixSquare(even, odd)
		if secondLength&1 != 0 {
			first = gf2MatrixTimes(even, first)
		}
		secondLength >>= 1
		if secondLength == 0 {
			break
		}
		gf2MatrixSquare(odd, even)
		if secondLength&1 != 0 {
			first = gf2MatrixTimes(odd, first)
		}
		secondLength >>= 1
		if secondLength == 0 {
			break
		}
	}
	return first ^ second
}

func gf2MatrixTimes[T crcWord](matrix []T, vector T) T {
	var sum T
	index := 0
	for vector != 0 {
		if vector&1 != 0 {
			sum ^= matrix[index]
		}
		vector >>= 1
		index++
	}
	return sum
}

func gf2MatrixSquare[T crcWord](square, matrix []T) {
	for index := range square {
		square[index] = gf2MatrixTimes(matrix, matrix[index])
	}
}
