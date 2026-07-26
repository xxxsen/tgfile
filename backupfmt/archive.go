package backupfmt

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // Backup compatibility fields preserve the existing MD5 protocol format.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/s3checksum"
)

var partEntryPattern = regexp.MustCompile(`^parts/(f[0-9]{8})/([0-9]{8})\.bin$`)

type PartOpenFunc func(
	ctx context.Context,
	fileRef string,
	partIndex int,
) (io.ReadCloser, error)

// PartVisitFunc consumes one physical part from an already verified archive.
// The reader is valid only for the duration of the callback and is bounded to
// the exact part size declared by the tar header.
type PartVisitFunc func(ctx context.Context, part Part, reader io.Reader) error

func Build(
	ctx context.Context,
	writer io.Writer,
	manifest *Manifest,
	limits Limits,
	openPart PartOpenFunc,
) error {
	if openPart == nil {
		return fmt.Errorf("part opener is nil: %w", ErrInvalidArchive)
	}
	if err := validateManifestForBuild(manifest, limits); err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(writer)
	gzipWriter.ModTime = time.Unix(0, 0)
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	closeWithError := func(cause error) error {
		return errors.Join(cause, tarWriter.Close(), gzipWriter.Close())
	}
	formatRaw, err := json.Marshal(FormatHeader{Format: FormatName, Version: FormatVersion})
	if err != nil {
		return closeWithError(fmt.Errorf("encode format header: %w", err))
	}
	if err := writeTarBytes(tarWriter, FormatEntry, formatRaw); err != nil {
		return closeWithError(err)
	}
	if err := writeArchiveParts(ctx, tarWriter, manifest, openPart); err != nil {
		return closeWithError(err)
	}
	if err := ValidateManifest(manifest, limits, manifest.Source.MaxPartSize); err != nil {
		return closeWithError(err)
	}
	manifestRaw, err := MarshalManifest(manifest)
	if err != nil {
		return closeWithError(err)
	}
	if int64(len(manifestRaw)) > limits.MaxManifestBytes {
		return closeWithError(limitExceeded("manifest bytes"))
	}
	if err := writeTarBytes(tarWriter, ManifestEntry, manifestRaw); err != nil {
		return closeWithError(err)
	}
	if err := tarWriter.Close(); err != nil {
		return errors.Join(fmt.Errorf("close backup tar stream: %w", err), gzipWriter.Close())
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("close backup gzip stream: %w", err)
	}
	return nil
}

func writeArchiveParts(
	ctx context.Context,
	writer *tar.Writer,
	manifest *Manifest,
	openPart PartOpenFunc,
) error {
	for fileIndex := range manifest.Files {
		if err := writeArchiveFile(ctx, writer, &manifest.Files[fileIndex], openPart); err != nil {
			return err
		}
	}
	return nil
}

func writeArchiveFile(
	ctx context.Context,
	writer *tar.Writer,
	file *File,
	openPart PartOpenFunc,
) error {
	for partIndex := range file.Parts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("build backup archive: %w", err)
		}
		part := &file.Parts[partIndex]
		stream, err := openPart(ctx, file.Ref, part.Index)
		if err != nil {
			return fmt.Errorf("open %s: %w", part.Entry, err)
		}
		copyErr := writePart(writer, part, stream)
		if err := errors.Join(copyErr, stream.Close()); err != nil {
			return err
		}
	}
	if file.LayoutVersion != 1 {
		return nil
	}
	computed := compatibilityMD5(*file)
	if file.CompatibilityMD5 != "" && file.CompatibilityMD5 != computed {
		return fmt.Errorf(
			"file %s compatibility MD5 differs from its parts: %w",
			file.Ref,
			ErrChecksum,
		)
	}
	file.CompatibilityMD5 = computed
	return nil
}

func validateManifestForBuild(manifest *Manifest, limits Limits) error {
	if manifest == nil {
		return invalidArchive("manifest is nil")
	}
	copyManifest := *manifest
	copyManifest.Files = make([]File, len(manifest.Files))
	for index := range manifest.Files {
		copyManifest.Files[index] = manifest.Files[index]
		copyManifest.Files[index].Parts = append([]Part(nil), manifest.Files[index].Parts...)
		for partIndex := range copyManifest.Files[index].Parts {
			part := &copyManifest.Files[index].Parts[partIndex]
			if part.SHA256 == "" {
				part.SHA256 = strings.Repeat("0", sha256.Size*2)
			}
			if part.MD5 == "" {
				part.MD5 = strings.Repeat("0", md5.Size*2)
			}
		}
		if copyManifest.Files[index].CompatibilityMD5 == "" &&
			copyManifest.Files[index].LayoutVersion == 1 {
			copyManifest.Files[index].CompatibilityMD5 = compatibilityMD5(
				copyManifest.Files[index],
			)
		} else if copyManifest.Files[index].CompatibilityMD5 == "" {
			copyManifest.Files[index].CompatibilityMD5 = strings.Repeat("0", md5.Size*2)
		}
	}
	return ValidateManifest(&copyManifest, limits, manifest.Source.MaxPartSize)
}

func writeTarBytes(writer *tar.Writer, name string, raw []byte) error {
	if err := writer.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(raw)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := writer.Write(raw); err != nil {
		return fmt.Errorf("write tar entry %s: %w", name, err)
	}
	return nil
}

func writePart(writer *tar.Writer, part *Part, stream io.Reader) error {
	if err := writer.WriteHeader(&tar.Header{
		Name:     part.Entry,
		Mode:     0o600,
		Size:     part.Size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return fmt.Errorf("write part header %s: %w", part.Entry, err)
	}
	md5Hash := md5.New() //nolint:gosec // Backup compatibility fields preserve the existing MD5 protocol format.
	shaHash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(writer, md5Hash, shaHash), stream, part.Size)
	if err != nil {
		return fmt.Errorf("write part %s after %d bytes: %w", part.Entry, written, err)
	}
	var extra [1]byte
	read, readErr := stream.Read(extra[:])
	if read != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return fmt.Errorf("part %s contains more than %d bytes: %w", part.Entry, part.Size, ErrInvalidArchive)
	}
	md5Value := hex.EncodeToString(md5Hash.Sum(nil))
	if part.MD5 != "" && md5Value != part.MD5 {
		return fmt.Errorf("part %s MD5 differs from metadata: %w", part.Entry, ErrChecksum)
	}
	part.MD5 = md5Value
	shaValue := hex.EncodeToString(shaHash.Sum(nil))
	if part.SHA256 != "" && part.SHA256 != shaValue {
		return fmt.Errorf("part %s SHA-256 differs from metadata: %w", part.Entry, ErrChecksum)
	}
	part.SHA256 = shaValue
	return nil
}

func compatibilityMD5(file File) string {
	if len(file.Parts) == 0 {
		return "d41d8cd98f00b204e9800998ecf8427e"
	}
	if len(file.Parts) == 1 {
		return file.Parts[0].MD5
	}
	hash := md5.New() //nolint:gosec // Persisted tgfile compatibility digest.
	for _, part := range file.Parts {
		_, _ = hash.Write([]byte(part.MD5))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func InspectFile(
	ctx context.Context,
	filename string,
	limits Limits,
	targetMaxPartSize int64,
) (*Manifest, *Report, error) {
	return readFile(ctx, filename, limits, targetMaxPartSize, false)
}

func VerifyFile(
	ctx context.Context,
	filename string,
	limits Limits,
	targetMaxPartSize int64,
) (*Manifest, *Report, error) {
	_, report, err := readFile(ctx, filename, limits, targetMaxPartSize, false)
	if err != nil {
		return nil, nil, err
	}
	verified, verifiedReport, err := readFile(ctx, filename, limits, targetMaxPartSize, true)
	if err != nil {
		return nil, nil, err
	}
	if verifiedReport.ArtifactBytes != report.ArtifactBytes ||
		verifiedReport.ArtifactSHA256 != report.ArtifactSHA256 {
		return nil, nil, fmt.Errorf("backup artifact changed during verification: %w", ErrChecksum)
	}
	return verified, verifiedReport, nil
}

// WalkParts streams every physical part in archive order. Callers must invoke
// VerifyFile first; this function deliberately avoids a second set of digest
// calculations so large imports remain linear rather than rescanning the
// archive once per part.
func WalkParts(
	ctx context.Context,
	filename string,
	limits Limits,
	visit PartVisitFunc,
) error {
	if visit == nil {
		return invalidArchive("part visitor is nil")
	}
	stat, err := os.Stat(filename)
	if err != nil {
		return fmt.Errorf("stat backup archive: %w", err)
	}
	if stat.Size() < 1 || stat.Size() > limits.MaxArchiveBytes {
		return limitExceeded("archive bytes")
	}
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open backup archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open backup gzip stream: %w", ErrInvalidArchive)
	}
	defer func() { _ = gzipReader.Close() }()
	gzipReader.Multistream(false)
	tarReader := tar.NewReader(gzipReader)
	seenFormat := false
	for {
		part, done, err := nextWalkPart(ctx, tarReader, &seenFormat)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if part == nil {
			continue
		}
		if err := visit(ctx, *part, io.LimitReader(tarReader, part.Size)); err != nil {
			return fmt.Errorf("consume backup part %s: %w", part.Entry, err)
		}
	}
}

func nextWalkPart(
	ctx context.Context,
	reader *tar.Reader,
	seenFormat *bool,
) (*Part, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("walk backup parts: %w", err)
	}
	header, err := reader.Next()
	if errors.Is(err, io.EOF) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read backup tar header: %w", ErrInvalidArchive)
	}
	if !*seenFormat {
		if header.Name != FormatEntry {
			return nil, false, invalidArchive("format header is missing")
		}
		*seenFormat = true
		return nil, false, nil
	}
	if header.Name == ManifestEntry {
		return nil, true, nil
	}
	matches := partEntryPattern.FindStringSubmatch(header.Name)
	if len(matches) != 3 {
		return nil, false, invalidArchive("tar entry name is not allowed")
	}
	index, err := strconv.Atoi(matches[2])
	if err != nil {
		return nil, false, invalidArchive("part index is invalid")
	}
	return &Part{Index: index, Size: header.Size, Entry: header.Name}, false, nil
}

type observedPart struct {
	size   int64
	md5    string
	sha256 string
}

type fileContentDigest struct {
	md5       string
	checksums map[s3checksum.Algorithm]string
}

type fileDigestAccumulator struct {
	ref       string
	md5       hash.Hash
	checksums map[s3checksum.Algorithm]hash.Hash
}

func readFile(
	ctx context.Context,
	filename string,
	limits Limits,
	targetMaxPartSize int64,
	verifyContent bool,
) (*Manifest, *Report, error) {
	stat, err := os.Stat(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("stat backup archive: %w", err)
	}
	if stat.Size() < 1 || stat.Size() > limits.MaxArchiveBytes {
		return nil, nil, limitExceeded("archive bytes")
	}
	artifactHash, err := fileSHA256(ctx, filename)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("open backup archive: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	manifest, observed, contentDigests, err := readArchiveStream(
		ctx,
		file,
		limits,
		verifyContent,
	)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateManifest(manifest, limits, targetMaxPartSize); err != nil {
		return nil, nil, err
	}
	if err := compareObservedParts(manifest, observed, verifyContent); err != nil {
		return nil, nil, err
	}
	if verifyContent {
		if err := validateContentMetadata(manifest, contentDigests); err != nil {
			return nil, nil, err
		}
	}
	return manifest, &Report{
		ArtifactSHA256: artifactHash,
		ArtifactBytes:  stat.Size(),
		Summary:        manifest.Limits,
	}, nil
}

func readArchiveStream(
	ctx context.Context,
	file io.Reader,
	limits Limits,
	verifyContent bool,
) (*Manifest, map[string]observedPart, map[string]fileContentDigest, error) {
	buffered := bufio.NewReader(file)
	gzipReader, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open backup gzip stream: %w", ErrInvalidArchive)
	}
	defer func() {
		_ = gzipReader.Close()
	}()
	gzipReader.Multistream(false)
	tarReader := tar.NewReader(gzipReader)
	manifest, observed, contentDigests, err := readTar(ctx, tarReader, limits, verifyContent)
	if err != nil {
		return nil, nil, nil, err
	}
	trailingBytes, err := io.Copy(
		io.Discard,
		io.LimitReader(gzipReader, limits.MaxExpandedBytes+1),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("finish backup gzip stream: %w", ErrInvalidArchive)
	}
	if trailingBytes > limits.MaxExpandedBytes {
		return nil, nil, nil, limitExceeded("expanded bytes")
	}
	if err := gzipReader.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close backup gzip stream: %w", ErrInvalidArchive)
	}
	if _, err := buffered.Peek(1); !errors.Is(err, io.EOF) {
		return nil, nil, nil, invalidArchive(
			"archive contains trailing data or another gzip member",
		)
	}
	return manifest, observed, contentDigests, nil
}

func readTar(
	ctx context.Context,
	reader *tar.Reader,
	limits Limits,
	verifyContent bool,
) (*Manifest, map[string]observedPart, map[string]fileContentDigest, error) {
	state := &tarReadState{
		observed:      make(map[string]observedPart),
		contentDigest: make(map[string]fileContentDigest),
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, fmt.Errorf("read backup archive: %w", err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read backup tar header: %w", ErrInvalidArchive)
		}
		if err := state.consume(reader, header, limits, verifyContent); err != nil {
			return nil, nil, nil, err
		}
	}
	if state.entryIndex == 0 || state.manifest == nil {
		return nil, nil, nil, invalidArchive("archive is missing format or manifest")
	}
	state.finishFileDigest()
	return state.manifest, state.observed, state.contentDigest, nil
}

type tarReadState struct {
	observed      map[string]observedPart
	contentDigest map[string]fileContentDigest
	fileDigest    *fileDigestAccumulator
	manifest      *Manifest
	entryIndex    int
	expanded      int64
	lastPart      string
}

func (s *tarReadState) consume(
	reader io.Reader,
	header *tar.Header,
	limits Limits,
	verifyContent bool,
) error {
	if header.Typeflag != tar.TypeReg ||
		header.Format != tar.FormatUSTAR ||
		len(header.PAXRecords) != 0 {
		return invalidArchive("tar entry type or format is not allowed")
	}
	if header.Size < 0 ||
		s.expanded > limits.MaxExpandedBytes-header.Size {
		return limitExceeded("expanded bytes")
	}
	s.expanded += header.Size
	if s.manifest != nil {
		return invalidArchive("manifest is not the last tar entry")
	}
	var err error
	switch {
	case s.entryIndex == 0:
		err = s.consumeFormat(reader, header)
	case header.Name == ManifestEntry:
		err = s.consumeManifest(reader, header, limits)
	default:
		err = s.consumePart(reader, header, limits, verifyContent)
	}
	if err == nil {
		s.entryIndex++
	}
	return err
}

func (s *tarReadState) consumeFormat(reader io.Reader, header *tar.Header) error {
	if header.Name != FormatEntry || header.Size < 1 || header.Size > 1024 {
		return invalidArchive("format header is missing or invalid")
	}
	raw, err := readExactEntry(reader, header.Size)
	if err != nil {
		return err
	}
	var format FormatHeader
	if err := decodeStrictJSON(raw, &format); err != nil ||
		format.Format != FormatName ||
		format.Version != FormatVersion {
		return invalidArchive("format header is invalid")
	}
	return nil
}

func (s *tarReadState) consumeManifest(
	reader io.Reader,
	header *tar.Header,
	limits Limits,
) error {
	s.finishFileDigest()
	if header.Size < 1 || header.Size > limits.MaxManifestBytes {
		return limitExceeded("manifest bytes")
	}
	raw, err := readExactEntry(reader, header.Size)
	if err != nil {
		return err
	}
	var decoded Manifest
	if err := decodeStrictJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode backup manifest: %w", ErrInvalidArchive)
	}
	s.manifest = &decoded
	return nil
}

func (s *tarReadState) consumePart(
	reader io.Reader,
	header *tar.Header,
	limits Limits,
	verifyContent bool,
) error {
	if !partEntryPattern.MatchString(header.Name) {
		return invalidArchive("tar entry name is not allowed")
	}
	if header.Name <= s.lastPart {
		return invalidArchive("tar part entries are not in canonical order")
	}
	if _, exists := s.observed[header.Name]; exists {
		return invalidArchive("tar entry is duplicated")
	}
	if header.Size < 0 || header.Size > limits.MaxExpandedBytes {
		return limitExceeded("part bytes")
	}
	matches := partEntryPattern.FindStringSubmatch(header.Name)
	var contentWriter io.Writer
	if verifyContent {
		contentWriter = s.fileContentWriter(matches[1])
	}
	part, err := observePart(reader, header.Size, verifyContent, contentWriter)
	if err != nil {
		return err
	}
	s.observed[header.Name] = part
	s.lastPart = header.Name
	if len(s.observed) > limits.MaxPartCount {
		return limitExceeded("part count")
	}
	return nil
}

func readExactEntry(reader io.Reader, size int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil {
		return nil, fmt.Errorf("read tar entry: %w", err)
	}
	if int64(len(raw)) != size {
		return nil, fmt.Errorf("tar entry is truncated: %w", ErrInvalidArchive)
	}
	return raw, nil
}

func observePart(
	reader io.Reader,
	size int64,
	verify bool,
	contentWriter io.Writer,
) (observedPart, error) {
	if !verify {
		written, err := io.CopyN(io.Discard, reader, size)
		if err != nil {
			return observedPart{}, fmt.Errorf("read part after %d bytes: %w", written, ErrInvalidArchive)
		}
		return observedPart{size: size}, nil
	}
	md5Hash := md5.New() //nolint:gosec // Backup compatibility fields preserve the existing MD5 protocol format.
	shaHash := sha256.New()
	writers := []io.Writer{md5Hash, shaHash}
	if contentWriter != nil {
		writers = append(writers, contentWriter)
	}
	written, err := io.CopyN(io.MultiWriter(writers...), reader, size)
	if err != nil {
		return observedPart{}, fmt.Errorf("hash part after %d bytes: %w", written, ErrInvalidArchive)
	}
	return observedPart{
		size:   size,
		md5:    hex.EncodeToString(md5Hash.Sum(nil)),
		sha256: hex.EncodeToString(shaHash.Sum(nil)),
	}, nil
}

func (s *tarReadState) fileContentWriter(fileRef string) io.Writer {
	if s.fileDigest == nil || s.fileDigest.ref != fileRef {
		s.finishFileDigest()
		s.fileDigest = newFileDigestAccumulator(fileRef)
	}
	writers := make([]io.Writer, 0, len(s.fileDigest.checksums)+1)
	writers = append(writers, s.fileDigest.md5)
	for _, algorithm := range supportedContentDigestAlgorithms() {
		writers = append(writers, s.fileDigest.checksums[algorithm])
	}
	return io.MultiWriter(writers...)
}

func newFileDigestAccumulator(fileRef string) *fileDigestAccumulator {
	accumulator := &fileDigestAccumulator{
		ref:       fileRef,
		md5:       md5.New(), //nolint:gosec // Persisted tgfile compatibility digest.
		checksums: make(map[s3checksum.Algorithm]hash.Hash),
	}
	for _, algorithm := range supportedContentDigestAlgorithms() {
		hasher, _ := s3checksum.NewHash(algorithm)
		accumulator.checksums[algorithm] = hasher
	}
	return accumulator
}

func (s *tarReadState) finishFileDigest() {
	if s.fileDigest == nil {
		return
	}
	s.contentDigest[s.fileDigest.ref] = accumulatedFileDigest(s.fileDigest)
	s.fileDigest = nil
}

func accumulatedFileDigest(accumulator *fileDigestAccumulator) fileContentDigest {
	digest := fileContentDigest{
		md5:       hex.EncodeToString(accumulator.md5.Sum(nil)),
		checksums: make(map[s3checksum.Algorithm]string, len(accumulator.checksums)),
	}
	for algorithm, hasher := range accumulator.checksums {
		digest.checksums[algorithm] = s3checksum.SumBase64(hasher)
	}
	return digest
}

func supportedContentDigestAlgorithms() []s3checksum.Algorithm {
	return []s3checksum.Algorithm{
		s3checksum.AlgorithmCRC32,
		s3checksum.AlgorithmCRC32C,
		s3checksum.AlgorithmCRC64NVME,
		s3checksum.AlgorithmSHA1,
		s3checksum.AlgorithmSHA256,
	}
}

func compareObservedParts(
	manifest *Manifest,
	observed map[string]observedPart,
	verifyContent bool,
) error {
	expected := 0
	for _, file := range manifest.Files {
		for _, part := range file.Parts {
			expected++
			actual, exists := observed[part.Entry]
			if !exists || actual.size != part.Size {
				return invalidArchive("manifest part entry is missing or has the wrong size")
			}
			if verifyContent && (actual.md5 != part.MD5 || actual.sha256 != part.SHA256) {
				return fmt.Errorf("part %s digest does not match manifest: %w", part.Entry, ErrChecksum)
			}
		}
	}
	if len(observed) != expected {
		return invalidArchive("archive contains an undeclared part entry")
	}
	return nil
}

func fileSHA256(ctx context.Context, filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open archive for checksum: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	value := sha256.New()
	if _, err := copyWithContext(ctx, value, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(value.Sum(nil)), nil
}

func copyWithContext(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("copy archive: %w", err)
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			written, writeErr := writer.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, fmt.Errorf("write archive: %w", writeErr)
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, fmt.Errorf("read archive: %w", readErr)
		}
	}
}
