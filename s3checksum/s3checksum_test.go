package s3checksum

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveMultipartDefaultsAndCombinations(t *testing.T) {
	tests := []struct {
		name      string
		algorithm string
		valueType string
		wantAlg   Algorithm
		wantType  Type
		wantError error
	}{
		{name: "default", wantAlg: AlgorithmCRC64NVME, wantType: TypeFullObject},
		{
			name:      "type only full",
			valueType: string(TypeFullObject),
			wantAlg:   AlgorithmCRC64NVME,
			wantType:  TypeFullObject,
		},
		{
			name:      "crc32 default composite",
			algorithm: string(AlgorithmCRC32),
			wantAlg:   AlgorithmCRC32,
			wantType:  TypeComposite,
		},
		{
			name:      "crc32 full",
			algorithm: string(AlgorithmCRC32),
			valueType: string(TypeFullObject),
			wantAlg:   AlgorithmCRC32,
			wantType:  TypeFullObject,
		},
		{
			name:      "sha256 composite",
			algorithm: string(AlgorithmSHA256),
			valueType: string(TypeComposite),
			wantAlg:   AlgorithmSHA256,
			wantType:  TypeComposite,
		},
		{
			name:      "composite without algorithm",
			valueType: string(TypeComposite),
			wantError: ErrInvalidCombination,
		},
		{
			name:      "crc64 composite",
			algorithm: string(AlgorithmCRC64NVME),
			valueType: string(TypeComposite),
			wantError: ErrInvalidCombination,
		},
		{
			name:      "sha1 full",
			algorithm: string(AlgorithmSHA1),
			valueType: string(TypeFullObject),
			wantError: ErrInvalidCombination,
		},
		{name: "known unsupported", algorithm: "XXHASH3", wantError: ErrUnsupportedAlgorithm},
		{name: "unknown", algorithm: "CRC16", wantError: ErrInvalidAlgorithm},
		{
			name:      "lowercase rejected",
			algorithm: "sha256",
			wantError: ErrInvalidAlgorithm,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			algorithm, checksumType, err := ResolveMultipart(test.algorithm, test.valueType)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.wantAlg, algorithm)
			require.Equal(t, test.wantType, checksumType)
		})
	}
}

func TestAlgorithmKnownVectors(t *testing.T) {
	tests := []struct {
		algorithm Algorithm
		input     string
		wantHex   string
	}{
		{algorithm: AlgorithmCRC32, input: "123456789", wantHex: "cbf43926"},
		{algorithm: AlgorithmCRC32C, input: "123456789", wantHex: "e3069283"},
		{algorithm: AlgorithmCRC64NVME, input: "hello world", wantHex: "8d29d5c3f6ea8ebe"},
		{algorithm: AlgorithmSHA1, input: "123456789", wantHex: "f7c3bc1d808e04732adf679965ccc34ca7ae3441"},
		{
			algorithm: AlgorithmSHA256,
			input:     "123456789",
			wantHex:   "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
		},
	}
	for _, test := range tests {
		hasher, err := NewHash(test.algorithm)
		require.NoError(t, err)
		_, err = hasher.Write([]byte(test.input))
		require.NoError(t, err)
		require.Equal(t, test.wantHex, hex.EncodeToString(hasher.Sum(nil)), test.algorithm)
	}
}

func TestDecodeRejectsMalformedValues(t *testing.T) {
	_, err := Decode(AlgorithmSHA256, "not-base64")
	require.ErrorIs(t, err, ErrInvalidDigest)
	_, err = Decode(AlgorithmSHA256, base64.StdEncoding.EncodeToString(make([]byte, 31)))
	require.ErrorIs(t, err, ErrInvalidDigest)
	_, err = Decode(Algorithm("CRC16"), "AAAA")
	require.ErrorIs(t, err, ErrInvalidAlgorithm)
}

func TestFullObjectCRCMatchesDirectHash(t *testing.T) {
	algorithms := []Algorithm{AlgorithmCRC32, AlgorithmCRC32C, AlgorithmCRC64NVME}
	data := deterministicChecksumTestBytes(256*1024, 20260725)
	splits := []int{0, 1, 2, 4095, 4096, 64 * 1024, len(data) - 1, len(data)}
	for _, algorithm := range algorithms {
		t.Run(string(algorithm), func(t *testing.T) {
			parts := make([]PartChecksum, 0, len(splits))
			start := 0
			for _, end := range splits {
				if end < start {
					continue
				}
				parts = append(parts, checksumPart(t, algorithm, data[start:end]))
				start = end
			}
			actual, err := FullObject(algorithm, parts)
			require.NoError(t, err)
			require.Equal(t, checksumValue(t, algorithm, data), actual)
		})
	}
}

func deterministicChecksumTestBytes(size int, seed uint64) []byte {
	result := make([]byte, 0, size)
	var counter uint64
	for len(result) < size {
		var input [16]byte
		binary.BigEndian.PutUint64(input[:8], seed)
		binary.BigEndian.PutUint64(input[8:], counter)
		block := sha256.Sum256(input[:])
		result = append(result, block[:]...)
		counter++
	}
	return result[:size]
}

func TestFullObjectCRCAcrossTelegramBoundary(t *testing.T) {
	const telegramPartSize = 20 * 1024 * 1024
	data := bytes.Repeat([]byte("tgfile-checksum"), telegramPartSize/len("tgfile-checksum")+2)
	data = data[:telegramPartSize+1]
	for _, algorithm := range []Algorithm{AlgorithmCRC32, AlgorithmCRC32C, AlgorithmCRC64NVME} {
		parts := []PartChecksum{
			checksumPart(t, algorithm, data[:telegramPartSize-1]),
			checksumPart(t, algorithm, nil),
			checksumPart(t, algorithm, data[telegramPartSize-1:]),
		}
		actual, err := FullObject(algorithm, parts)
		require.NoError(t, err)
		require.Equal(t, checksumValue(t, algorithm, data), actual)
	}
}

func TestCompositeUsesRawDigestBytes(t *testing.T) {
	values := []string{
		checksumValue(t, AlgorithmSHA256, []byte("part-one")),
		checksumValue(t, AlgorithmSHA256, []byte("part-two")),
	}
	raw := make([]byte, 0, 64)
	for _, value := range values {
		decoded, err := base64.StdEncoding.DecodeString(value)
		require.NoError(t, err)
		raw = append(raw, decoded...)
	}
	wantRequest := checksumValue(t, AlgorithmSHA256, raw)
	requestValue, storedValue, err := Composite(AlgorithmSHA256, values)
	require.NoError(t, err)
	require.Equal(t, wantRequest, requestValue)
	require.Equal(t, wantRequest+"-2", storedValue)
	parsed, err := ParseCompositeStored(AlgorithmSHA256, storedValue, 2)
	require.NoError(t, err)
	require.Equal(t, wantRequest, parsed)
}

func checksumPart(t *testing.T, algorithm Algorithm, data []byte) PartChecksum {
	t.Helper()
	return PartChecksum{Value: checksumValue(t, algorithm, data), Size: int64(len(data))}
}

func checksumValue(t *testing.T, algorithm Algorithm, data []byte) string {
	t.Helper()
	hasher, err := NewHash(algorithm)
	require.NoError(t, err)
	_, err = hasher.Write(data)
	require.NoError(t, err)
	return SumBase64(hasher)
}
