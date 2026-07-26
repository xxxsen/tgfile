package backupfmt

import (
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xxxsen/tgfile/s3checksum"
)

const emptyMD5 = "d41d8cd98f00b204e9800998ecf8427e"

var (
	fileRefPattern    = regexp.MustCompile(`^f[0-9]{8}$`)
	bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

func ValidateManifest(manifest *Manifest, limits Limits, targetMaxPartSize int64) error {
	if err := validateManifestHeader(manifest, limits); err != nil {
		return err
	}
	if targetMaxPartSize <= 0 {
		targetMaxPartSize = math.MaxInt64
	}
	return validateManifestRecords(manifest, limits, targetMaxPartSize)
}

func validateManifestHeader(manifest *Manifest, limits Limits) error {
	if manifest == nil {
		return invalidArchive("manifest is nil")
	}
	if manifest.Format != FormatName || manifest.Version != FormatVersion {
		return invalidArchive("manifest format or version is not supported")
	}
	if _, err := time.Parse(time.RFC3339, manifest.CreatedAt); err != nil {
		return invalidArchive("created_at is not RFC3339")
	}
	if err := validatePath(manifest.Scope, limits.MaxPathBytes, true); err != nil {
		return fmt.Errorf("validate scope: %w", err)
	}
	if manifest.Source.SchemaVersion < 1 ||
		manifest.Source.BlockIOKind == "" ||
		manifest.Source.MaxPartSize < 1 {
		return invalidArchive("source metadata is invalid")
	}
	return validateCounts(manifest, limits)
}

func validateManifestRecords(
	manifest *Manifest,
	limits Limits,
	targetMaxPartSize int64,
) error {
	maxPartSize := min(targetMaxPartSize, manifest.Source.MaxPartSize)
	fileByRef, partCount, physicalBytes, err := validateFiles(
		manifest.Files,
		limits,
		maxPartSize,
	)
	if err != nil {
		return err
	}
	directories, err := validateDirectories(manifest.Directories, limits)
	if err != nil {
		return err
	}
	mappings, err := validateMappings(manifest.Mappings, fileByRef, directories, limits)
	if err != nil {
		return err
	}
	if err := validateScopeContents(manifest.Scope, manifest.Directories, manifest.Mappings); err != nil {
		return err
	}
	if err := validateProtocolMetadata(
		manifest,
		manifest.WebDAVProperties,
		mappings,
		directories,
		limits,
	); err != nil {
		return err
	}
	return validateManifestSummary(manifest, partCount, physicalBytes)
}

func validateProtocolMetadata(
	manifest *Manifest,
	properties []WebDAVProperty,
	mappings map[string]struct{},
	directories map[string]struct{},
	limits Limits,
) error {
	if err := validateBuckets(manifest.RequiredBuckets); err != nil {
		return err
	}
	if err := validateS3Objects(
		manifest.S3Objects,
		mappings,
		manifest.RequiredBuckets,
		limits,
	); err != nil {
		return err
	}
	return validateWebDAVProperties(properties, mappings, directories, limits)
}

func validateManifestSummary(manifest *Manifest, partCount, physicalBytes int64) error {
	actual := Summary{
		MappingCount:   int64(len(manifest.Mappings)),
		DirectoryCount: int64(len(manifest.Directories)),
		FileCount:      int64(len(manifest.Files)),
		PartCount:      partCount,
		PhysicalBytes:  physicalBytes,
	}
	if manifest.Limits != actual {
		return invalidArchive("manifest summary does not match its records")
	}
	return nil
}

func validateCounts(manifest *Manifest, limits Limits) error {
	if len(manifest.Mappings) > limits.MaxMappingCount {
		return limitExceeded("mapping count")
	}
	if len(manifest.Files) > limits.MaxFileCount {
		return limitExceeded("file count")
	}
	if len(manifest.Directories) > limits.MaxMappingCount {
		return limitExceeded("directory count")
	}
	return nil
}

func validateFiles(
	files []File,
	limits Limits,
	targetMaxPartSize int64,
) (map[string]File, int64, int64, error) {
	fileByRef := make(map[string]File, len(files))
	var partCount, physicalBytes int64
	lastRef := ""
	for fileIndex := range files {
		file := files[fileIndex]
		if file.Ref <= lastRef {
			return nil, 0, 0, invalidArchive("files are not in canonical order")
		}
		if err := validateFileRecord(file, fileByRef, targetMaxPartSize); err != nil {
			return nil, 0, 0, err
		}
		var err error
		partCount, physicalBytes, err = addPhysicalParts(
			file.Parts,
			partCount,
			physicalBytes,
			limits,
		)
		if err != nil {
			return nil, 0, 0, err
		}
		fileByRef[file.Ref] = file
		lastRef = file.Ref
	}
	if err := validateCompositeReferences(files, fileByRef); err != nil {
		return nil, 0, 0, err
	}
	return fileByRef, partCount, physicalBytes, nil
}

func validateFileRecord(
	file File,
	existing map[string]File,
	targetMaxPartSize int64,
) error {
	if !fileRefPattern.MatchString(file.Ref) {
		return invalidArchive("file_ref is invalid")
	}
	if _, exists := existing[file.Ref]; exists {
		return invalidArchive("file_ref is duplicated")
	}
	sourceID, sourceErr := strconv.ParseUint(file.SourceFileID, 10, 64)
	if sourceErr != nil || sourceID == 0 ||
		file.Size < 0 ||
		!isHex(file.CompatibilityMD5, 16) {
		return invalidArchive("file metadata is invalid")
	}
	switch file.LayoutVersion {
	case 1:
		return validatePhysicalFile(file, targetMaxPartSize)
	case 2:
		return validateCompositeShape(file)
	default:
		return invalidArchive("file layout is invalid")
	}
}

func addPhysicalParts(
	parts []Part,
	partCount, physicalBytes int64,
	limits Limits,
) (int64, int64, error) {
	for _, part := range parts {
		if partCount == math.MaxInt64 || physicalBytes > math.MaxInt64-part.Size {
			return 0, 0, limitExceeded("physical byte count overflow")
		}
		partCount++
		physicalBytes += part.Size
		if partCount > int64(limits.MaxPartCount) {
			return 0, 0, limitExceeded("part count")
		}
		if physicalBytes > limits.MaxExpandedBytes {
			return 0, 0, limitExceeded("expanded bytes")
		}
	}
	return partCount, physicalBytes, nil
}

func validatePhysicalFile(file File, targetMaxPartSize int64) error {
	if len(file.Segments) != 0 || len(file.CompletedParts) != 0 {
		return invalidArchive("layout v1 file contains composite records")
	}
	if file.Size == 0 {
		if len(file.Parts) != 0 || file.CompatibilityMD5 != emptyMD5 {
			return invalidArchive("empty file metadata is invalid")
		}
		return nil
	}
	if len(file.Parts) == 0 {
		return invalidArchive("non-empty physical file has no parts")
	}
	var total int64
	for index, part := range file.Parts {
		if err := validatePhysicalPart(file.Ref, part, index, targetMaxPartSize, total); err != nil {
			return err
		}
		total += part.Size
	}
	if total != file.Size {
		return invalidArchive("physical part sizes do not match file size")
	}
	if compatibilityMD5(file) != file.CompatibilityMD5 {
		return fmt.Errorf("physical file compatibility MD5 differs: %w", ErrChecksum)
	}
	return nil
}

func validatePhysicalPart(
	fileRef string,
	part Part,
	index int,
	targetMaxPartSize, currentSize int64,
) error {
	expectedEntry := fmt.Sprintf("parts/%s/%08d.bin", fileRef, index)
	if part.Index != index ||
		part.Size <= 0 ||
		part.Size > targetMaxPartSize ||
		!isHex(part.MD5, 16) ||
		!isHex(part.SHA256, 32) ||
		part.Entry != expectedEntry {
		return invalidArchive("physical part metadata is invalid")
	}
	if currentSize > math.MaxInt64-part.Size {
		return limitExceeded("file size overflow")
	}
	return nil
}

func validateCompositeShape(file File) error {
	if len(file.Parts) != 0 || len(file.Segments) == 0 {
		return invalidArchive("layout v2 file shape is invalid")
	}
	var total int64
	for index, segment := range file.Segments {
		if segment.Index != index || !fileRefPattern.MatchString(segment.SourceRef) || segment.Size < 0 {
			return invalidArchive("composite segment metadata is invalid")
		}
		if total > math.MaxInt64-segment.Size {
			return limitExceeded("composite size overflow")
		}
		total += segment.Size
	}
	if total != file.Size {
		return invalidArchive("segment sizes do not match composite size")
	}
	if len(file.CompletedParts) != len(file.Segments) {
		return invalidArchive("completed part count does not match segment count")
	}
	var completedSize int64
	for index, part := range file.CompletedParts {
		if part.PartNumber != index+1 || part.PartSize != file.Segments[index].Size {
			return invalidArchive("completed part order or size is invalid")
		}
		if err := validateCompletedChecksum(part); err != nil {
			return err
		}
		completedSize += part.PartSize
	}
	if completedSize != file.Size {
		return invalidArchive("completed part sizes do not match composite size")
	}
	return nil
}

func validateCompletedChecksum(part CompletedPart) error {
	switch part.ChecksumState {
	case "unavailable":
		if part.ChecksumAlgorithm != "" || part.ChecksumValue != "" {
			return invalidArchive("unavailable completed checksum contains a value")
		}
	case "available":
		algorithm, err := s3checksum.ParseAlgorithm(part.ChecksumAlgorithm)
		if err != nil || part.ChecksumValue == "" {
			return invalidArchive("available completed checksum is invalid")
		}
		if _, err := s3checksum.Decode(algorithm, part.ChecksumValue); err != nil {
			return invalidArchive("available completed checksum value is invalid")
		}
	default:
		return invalidArchive("completed checksum state is invalid")
	}
	return nil
}

func validateCompositeReferences(files []File, fileByRef map[string]File) error {
	for _, file := range files {
		if file.LayoutVersion != 2 {
			continue
		}
		seenSources := make(map[string]struct{}, len(file.Segments))
		for _, segment := range file.Segments {
			source, exists := fileByRef[segment.SourceRef]
			if !exists || source.LayoutVersion != 1 || source.Size != segment.Size {
				return invalidArchive("composite source is missing or incompatible")
			}
			if _, exists := seenSources[segment.SourceRef]; exists {
				return invalidArchive("composite source is repeated")
			}
			seenSources[segment.SourceRef] = struct{}{}
		}
	}
	return nil
}

func validateDirectories(items []Directory, limits Limits) (map[string]struct{}, error) {
	directories := make(map[string]struct{}, len(items))
	lastPath := ""
	for _, item := range items {
		if item.Path == "/" {
			return nil, invalidArchive("root directory must not be stored")
		}
		if err := validatePath(item.Path, limits.MaxPathBytes, true); err != nil {
			return nil, err
		}
		if _, exists := directories[item.Path]; exists {
			return nil, invalidArchive("directory path is duplicated")
		}
		if item.Path <= lastPath || item.Mode > 0o777 {
			return nil, invalidArchive("directory metadata is not canonical")
		}
		directories[item.Path] = struct{}{}
		lastPath = item.Path
	}
	for directoryPath := range directories {
		parent := path.Dir(directoryPath)
		if parent != "/" {
			if _, exists := directories[parent]; !exists {
				return nil, invalidArchive("directory parent is missing")
			}
		}
	}
	return directories, nil
}

func validateMappings(
	items []Mapping,
	files map[string]File,
	directories map[string]struct{},
	limits Limits,
) (map[string]struct{}, error) {
	mappings := make(map[string]struct{}, len(items))
	lastPath := ""
	for _, item := range items {
		if err := validatePath(item.Path, limits.MaxPathBytes, false); err != nil {
			return nil, err
		}
		file, exists := files[item.FileRef]
		if !exists || file.Size != item.Size {
			return nil, invalidArchive("mapping file reference or size is invalid")
		}
		if _, exists := mappings[item.Path]; exists {
			return nil, invalidArchive("mapping path is duplicated")
		}
		if item.Path <= lastPath || item.Mode > 0o777 {
			return nil, invalidArchive("mapping metadata is not canonical")
		}
		if _, exists := directories[item.Path]; exists {
			return nil, invalidArchive("mapping conflicts with a directory")
		}
		parent := path.Dir(item.Path)
		if parent != "/" {
			if _, exists := directories[parent]; !exists {
				return nil, invalidArchive("mapping parent directory is missing")
			}
		}
		mappings[item.Path] = struct{}{}
		lastPath = item.Path
	}
	return mappings, nil
}

func validateScopeContents(scope string, directories []Directory, mappings []Mapping) error {
	for _, item := range mappings {
		if !pathWithinScope(item.Path, scope) {
			return invalidArchive("mapping lies outside the declared scope")
		}
	}
	for _, item := range directories {
		if !pathWithinScope(item.Path, scope) && !pathWithinScope(scope, item.Path) {
			return invalidArchive("directory lies outside the declared scope dependencies")
		}
	}
	return nil
}

func pathWithinScope(value, scope string) bool {
	return scope == "/" || value == scope || strings.HasPrefix(value, scope+"/")
}

func validateS3Objects(
	items []S3Object,
	mappings map[string]struct{},
	buckets []RequiredBucket,
	limits Limits,
) error {
	required := make(map[string]struct{})
	for mappingPath := range mappings {
		if pathUsesBucket(mappingPath, buckets) {
			required[mappingPath] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(items))
	lastPath := ""
	for _, item := range items {
		if _, exists := mappings[item.Path]; !exists {
			return invalidArchive("S3 metadata path has no mapping")
		}
		if _, exists := required[item.Path]; !exists {
			return invalidArchive("S3 metadata path has no required bucket")
		}
		if _, exists := seen[item.Path]; exists {
			return invalidArchive("S3 metadata path is duplicated")
		}
		if item.Path <= lastPath {
			return invalidArchive("S3 metadata is not in canonical order")
		}
		if err := validateS3Metadata(item, limits); err != nil {
			return err
		}
		seen[item.Path] = struct{}{}
		lastPath = item.Path
	}
	if len(seen) != len(required) {
		return invalidArchive("S3 metadata is missing for a bucket mapping")
	}
	return nil
}

func pathUsesBucket(value string, buckets []RequiredBucket) bool {
	for _, bucket := range buckets {
		if strings.HasPrefix(value, "/"+bucket.Name+"/") {
			return true
		}
	}
	return false
}

func validateS3Metadata(item S3Object, limits Limits) error {
	if item.ETag == "" || containsControl(item.ETag) {
		return invalidArchive("S3 ETag is invalid")
	}
	if err := validateS3ResponseMetadata(item); err != nil {
		return err
	}
	if err := validateS3RequestChecksum(item); err != nil {
		return err
	}
	return validateS3UserMetadata(item.UserMetadata, limits)
}

func validateS3ResponseMetadata(item S3Object) error {
	for _, value := range []string{
		item.ContentType,
		item.CacheControl,
		item.ContentDisposition,
		item.ContentEncoding,
		item.ContentLanguage,
		item.Expires,
	} {
		if containsControl(value) {
			return invalidArchive("S3 response metadata contains a control character")
		}
	}
	if item.Expires != "" {
		if _, err := http.ParseTime(item.Expires); err != nil {
			return invalidArchive("S3 Expires metadata is invalid")
		}
	}
	if item.ChecksumSHA256 != "" {
		if _, err := s3checksum.Decode(s3checksum.AlgorithmSHA256, item.ChecksumSHA256); err != nil {
			return invalidArchive("S3 SHA-256 checksum is invalid")
		}
	}
	return nil
}

func validateS3UserMetadata(raw string, limits Limits) error {
	if len(raw) > limits.MaxUserMetaBytes {
		return limitExceeded("S3 user metadata")
	}
	var metadata map[string]string
	if err := decodeStrictJSON([]byte(raw), &metadata); err != nil {
		return invalidArchive("S3 user metadata is invalid JSON")
	}
	var total int
	for key, value := range metadata {
		if key == "" || len(key) > 128 || len(value) > 2*1024 ||
			containsControl(key) || containsControl(value) {
			return invalidArchive("S3 user metadata entry is invalid")
		}
		total += len(key) + len(value)
	}
	if total > limits.MaxUserMetaBytes {
		return limitExceeded("S3 user metadata")
	}
	return nil
}

func validateS3RequestChecksum(item S3Object) error {
	if item.RequestChecksumAlgorithm == "" &&
		item.RequestChecksumValue == "" &&
		item.ChecksumType == "" {
		return nil
	}
	if item.RequestChecksumAlgorithm == "" ||
		item.RequestChecksumValue == "" ||
		item.ChecksumType == "" {
		return invalidArchive("S3 request checksum tuple is incomplete")
	}
	algorithm, err := s3checksum.ParseAlgorithm(item.RequestChecksumAlgorithm)
	if err != nil {
		return invalidArchive("S3 request checksum algorithm is invalid")
	}
	if _, err := s3checksum.Decode(algorithm, item.RequestChecksumValue); err != nil {
		return invalidArchive("S3 request checksum value is invalid")
	}
	checksumType, err := s3checksum.ParseType(item.ChecksumType)
	if err != nil {
		return invalidArchive("S3 request checksum type is invalid")
	}
	if checksumType == s3checksum.TypeComposite {
		if err := s3checksum.ValidateCombination(algorithm, checksumType); err != nil {
			return invalidArchive("S3 request checksum combination is invalid")
		}
	}
	return nil
}

func validateWebDAVProperties(
	items []WebDAVProperty,
	mappings map[string]struct{},
	directories map[string]struct{},
	limits Limits,
) error {
	seen := make(map[string]struct{}, len(items))
	lastKey := ""
	for _, item := range items {
		_, isFile := mappings[item.Path]
		_, isDirectory := directories[item.Path]
		if !isFile && !isDirectory {
			return invalidArchive("WebDAV property path has no resource")
		}
		if item.NamespaceURI == "DAV:" ||
			strings.TrimSpace(item.LocalName) == "" ||
			!validXMLLocalName(item.LocalName) {
			return invalidArchive("WebDAV dead property name is invalid")
		}
		if containsControl(item.NamespaceURI) || containsControl(item.LocalName) {
			return invalidArchive("WebDAV dead property name contains a control character")
		}
		if len(item.ValueXML) > limits.MaxPropertyBytes {
			return limitExceeded("WebDAV property")
		}
		if err := validateXMLFragment(item.ValueXML); err != nil {
			return invalidArchive("WebDAV dead property XML is invalid")
		}
		key := item.Path + "\x00" + item.NamespaceURI + "\x00" + item.LocalName
		if _, exists := seen[key]; exists {
			return invalidArchive("WebDAV property is duplicated")
		}
		if key <= lastKey {
			return invalidArchive("WebDAV properties are not in canonical order")
		}
		seen[key] = struct{}{}
		lastKey = key
	}
	return nil
}

func validateXMLFragment(value string) error {
	decoder := xml.NewDecoder(strings.NewReader("<wrapper>" + value + "</wrapper>"))
	for {
		if _, err := decoder.Token(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func validXMLLocalName(value string) bool {
	decoder := xml.NewDecoder(strings.NewReader("<" + value + "></" + value + ">"))
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	start, ok := token.(xml.StartElement)
	if !ok || start.Name.Space != "" || start.Name.Local != value {
		return false
	}
	token, err = decoder.Token()
	if err != nil {
		return false
	}
	end, ok := token.(xml.EndElement)
	if !ok || end.Name != start.Name {
		return false
	}
	_, err = decoder.Token()
	return errors.Is(err, io.EOF)
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func validateBuckets(items []RequiredBucket) error {
	seen := make(map[string]struct{}, len(items))
	lastName := ""
	for _, item := range items {
		if !bucketNamePattern.MatchString(item.Name) ||
			strings.Contains(item.Name, "..") ||
			(item.ACL != "private" && item.ACL != "public-read") {
			return invalidArchive("required bucket is invalid")
		}
		if _, exists := seen[item.Name]; exists {
			return invalidArchive("required bucket is duplicated")
		}
		if item.Name <= lastName {
			return invalidArchive("required buckets are not in canonical order")
		}
		seen[item.Name] = struct{}{}
		lastName = item.Name
	}
	return nil
}

func validatePath(value string, maxBytes int, allowRoot bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return invalidArchive("path is empty, too long, or invalid UTF-8")
	}
	if !strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.Contains(value, "\\") {
		return invalidArchive("path is not canonical")
	}
	if value == "/" && !allowRoot {
		return invalidArchive("file path cannot be root")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return invalidArchive("path contains a control character")
		}
	}
	return nil
}

func validateContentMetadata(
	manifest *Manifest,
	contentDigests map[string]fileContentDigest,
) error {
	files := make(map[string]File, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Ref] = file
		if file.LayoutVersion != 1 {
			continue
		}
		if file.Size == 0 {
			contentDigests[file.Ref] = emptyContentDigest(file.Ref)
			continue
		}
		if _, exists := contentDigests[file.Ref]; !exists {
			return invalidArchive("physical file content digest is missing")
		}
	}
	if err := validateCompletedPartContent(manifest.Files, contentDigests); err != nil {
		return err
	}
	mappingRefs := make(map[string]string, len(manifest.Mappings))
	for _, mapping := range manifest.Mappings {
		mappingRefs[mapping.Path] = mapping.FileRef
	}
	for _, object := range manifest.S3Objects {
		if err := validateS3ObjectContent(
			object,
			files[mappingRefs[object.Path]],
			contentDigests,
		); err != nil {
			return err
		}
	}
	return nil
}

func emptyContentDigest(fileRef string) fileContentDigest {
	return accumulatedFileDigest(newFileDigestAccumulator(fileRef))
}

func validateCompletedPartContent(
	files []File,
	contentDigests map[string]fileContentDigest,
) error {
	for _, file := range files {
		if file.LayoutVersion != 2 {
			continue
		}
		for index, part := range file.CompletedParts {
			if part.ChecksumState != "available" {
				continue
			}
			algorithm, err := s3checksum.ParseAlgorithm(part.ChecksumAlgorithm)
			if err != nil {
				return invalidArchive("completed part checksum algorithm is invalid")
			}
			actual, exists := contentDigests[file.Segments[index].SourceRef]
			if !exists || actual.checksums[algorithm] != part.ChecksumValue {
				return fmt.Errorf(
					"completed part %d content checksum differs: %w",
					part.PartNumber,
					ErrChecksum,
				)
			}
		}
	}
	return nil
}

func validateS3ObjectContent(
	object S3Object,
	file File,
	contentDigests map[string]fileContentDigest,
) error {
	if object.RequestChecksumAlgorithm == "" {
		return nil
	}
	algorithm, err := s3checksum.ParseAlgorithm(object.RequestChecksumAlgorithm)
	if err != nil {
		return invalidArchive("S3 object checksum algorithm is invalid")
	}
	var expected string
	switch file.LayoutVersion {
	case 1:
		if object.ChecksumType != string(s3checksum.TypeFullObject) {
			return invalidArchive("physical S3 object checksum type is invalid")
		}
		expected = contentDigests[file.Ref].checksums[algorithm]
	case 2:
		expected, err = compositeObjectChecksum(file, algorithm, object.ChecksumType)
		if err != nil {
			return err
		}
	default:
		return invalidArchive("S3 object references an unsupported file layout")
	}
	if expected != object.RequestChecksumValue {
		return fmt.Errorf("S3 object content checksum differs: %w", ErrChecksum)
	}
	return nil
}

func compositeObjectChecksum(
	file File,
	algorithm s3checksum.Algorithm,
	checksumType string,
) (string, error) {
	values := make([]string, 0, len(file.CompletedParts))
	parts := make([]s3checksum.PartChecksum, 0, len(file.CompletedParts))
	for _, completed := range file.CompletedParts {
		if completed.ChecksumState != "available" ||
			completed.ChecksumAlgorithm != string(algorithm) {
			return "", invalidArchive("composite S3 checksum has incompatible completed parts")
		}
		values = append(values, completed.ChecksumValue)
		parts = append(parts, s3checksum.PartChecksum{
			Value: completed.ChecksumValue,
			Size:  completed.PartSize,
		})
	}
	switch s3checksum.Type(checksumType) {
	case s3checksum.TypeFullObject:
		value, err := s3checksum.FullObject(algorithm, parts)
		if err != nil {
			return "", invalidArchive("full-object S3 checksum is invalid")
		}
		return value, nil
	case s3checksum.TypeComposite:
		_, stored, err := s3checksum.Composite(algorithm, values)
		if err != nil {
			return "", invalidArchive("composite S3 checksum is invalid")
		}
		return stored, nil
	default:
		return "", invalidArchive("composite S3 object checksum type is invalid")
	}
}

func isHex(value string, bytes int) bool {
	if len(value) != bytes*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}

func invalidArchive(message string) error {
	return fmt.Errorf("%s: %w", message, ErrInvalidArchive)
}

func limitExceeded(name string) error {
	return fmt.Errorf("%s exceeds its configured limit: %w", name, ErrLimitExceeded)
}

func MarshalManifest(manifest *Manifest) ([]byte, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return raw, nil
}
