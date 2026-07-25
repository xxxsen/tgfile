package filemgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/xxxsen/tgfile/constant"
)

var (
	ErrInvalidFileLayout = errors.New("invalid file layout")
	ErrInvalidComposite  = errors.New("invalid composite file manifest")
)

type compositeSegment struct {
	index        int
	sourceFileID uint64
	size         int64
	start        int64
}

type compositeFileStream struct {
	ctx          context.Context
	manager      *defaultFileManager
	segments     []compositeSegment
	size         int64
	offset       int64
	current      io.ReadSeekCloser
	currentIndex int
	open         bool
}

func (d *defaultFileManager) compositeIOStream(
	fileID uint64,
	fileSize int64,
) func(context.Context) (io.ReadSeekCloser, error) {
	return func(ctx context.Context) (io.ReadSeekCloser, error) {
		segments, err := d.loadCompositeManifest(ctx, fileID, fileSize)
		if err != nil {
			return nil, err
		}
		return &compositeFileStream{
			ctx:          ctx,
			manager:      d,
			segments:     segments,
			size:         fileSize,
			currentIndex: -1,
			open:         true,
		}, nil
	}
}

func (d *defaultFileManager) loadCompositeManifest(
	ctx context.Context,
	fileID uint64,
	fileSize int64,
) ([]compositeSegment, error) {
	const query = `SELECT segment_index, source_file_id, segment_size,
f.file_size, f.file_state, f.file_layout_version
FROM tg_s3_file_segment_tab s
LEFT JOIN tg_file_tab f ON f.file_id = s.source_file_id
WHERE s.file_id = ?
ORDER BY segment_index`
	rows, err := d.dbc.QueryContext(ctx, query, fileID)
	if err != nil {
		return nil, fmt.Errorf("query composite manifest: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	segments := make([]compositeSegment, 0)
	var total int64
	for rows.Next() {
		var (
			index        int
			sourceFileID uint64
			segmentSize  int64
			sourceSize   sql.NullInt64
			sourceState  sql.NullInt64
			sourceLayout sql.NullInt64
		)
		if err := rows.Scan(
			&index,
			&sourceFileID,
			&segmentSize,
			&sourceSize,
			&sourceState,
			&sourceLayout,
		); err != nil {
			return nil, fmt.Errorf("scan composite segment: %w", err)
		}
		if err := validateCompositeSegment(
			index,
			len(segments),
			sourceFileID,
			segmentSize,
			sourceSize,
			sourceState,
			sourceLayout,
			total,
			fileSize,
		); err != nil {
			return nil, err
		}
		segments = append(segments, compositeSegment{
			index:        index,
			sourceFileID: sourceFileID,
			size:         segmentSize,
			start:        total,
		})
		total += segmentSize
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate composite manifest: %w", err)
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: manifest is empty", ErrInvalidComposite)
	}
	if total != fileSize {
		return nil, fmt.Errorf("%w: manifest size=%d final size=%d", ErrInvalidComposite, total, fileSize)
	}
	return segments, nil
}

func validateCompositeSegment(
	index, expectedIndex int,
	sourceFileID uint64,
	segmentSize int64,
	sourceSize, sourceState, sourceLayout sql.NullInt64,
	total, fileSize int64,
) error {
	if index != expectedIndex {
		return fmt.Errorf("%w: segment index=%d expected=%d", ErrInvalidComposite, index, expectedIndex)
	}
	if !sourceSize.Valid || !sourceState.Valid || !sourceLayout.Valid {
		return fmt.Errorf("%w: source file %d does not exist", ErrInvalidComposite, sourceFileID)
	}
	if sourceState.Int64 != constant.FileStateReady {
		return fmt.Errorf("%w: source file %d is not ready", ErrInvalidComposite, sourceFileID)
	}
	if sourceLayout.Int64 != 1 {
		return fmt.Errorf("%w: source file %d has layout %d", ErrInvalidComposite, sourceFileID, sourceLayout.Int64)
	}
	if sourceSize.Int64 != segmentSize {
		return fmt.Errorf(
			"%w: source file %d size=%d segment=%d",
			ErrInvalidComposite,
			sourceFileID,
			sourceSize.Int64,
			segmentSize,
		)
	}
	if segmentSize < 0 || total > fileSize-segmentSize {
		return fmt.Errorf("%w: composite size exceeds final size", ErrInvalidComposite)
	}
	return nil
}

func (f *compositeFileStream) Read(buffer []byte) (int, error) {
	if !f.open {
		return 0, ErrFileNotOpen
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if f.offset == f.size {
		return 0, io.EOF
	}
	totalRead := 0
	for totalRead < len(buffer) && f.offset < f.size {
		index := f.segmentAt(f.offset)
		if index < 0 {
			return totalRead, fmt.Errorf("%w: no segment at offset %d", ErrInvalidComposite, f.offset)
		}
		count, err := f.readSegment(index, buffer[totalRead:])
		totalRead += count
		if err != nil {
			return totalRead, err
		}
	}
	return totalRead, nil
}

func (f *compositeFileStream) readSegment(index int, buffer []byte) (int, error) {
	segment := f.segments[index]
	if err := f.openSegment(index, segment); err != nil {
		return 0, err
	}
	remaining := segment.start + segment.size - f.offset
	requestSize := min(int64(len(buffer)), remaining)
	count, readErr := f.current.Read(buffer[:int(requestSize)])
	f.offset += int64(count)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return count, fmt.Errorf("read composite source %d: %w", segment.sourceFileID, readErr)
	}
	if count == 0 && f.offset < segment.start+segment.size {
		if err := f.advancePhysicalBoundary(segment); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if f.offset == segment.start+segment.size {
		return count, f.closeCurrent()
	}
	return count, nil
}

func (f *compositeFileStream) advancePhysicalBoundary(segment compositeSegment) error {
	sourceOffset := f.offset - segment.start
	blockSize := f.manager.bkio.MaxFileSize()
	if blockSize <= 0 || sourceOffset <= 0 || sourceOffset%blockSize != 0 {
		return fmt.Errorf(
			"%w: source file %d ended at %d of %d",
			io.ErrUnexpectedEOF,
			segment.sourceFileID,
			sourceOffset,
			segment.size,
		)
	}
	if _, err := f.current.Seek(sourceOffset, io.SeekStart); err != nil {
		return fmt.Errorf("advance composite source %d: %w", segment.sourceFileID, err)
	}
	return nil
}

func (f *compositeFileStream) segmentAt(offset int64) int {
	index := sort.Search(len(f.segments), func(index int) bool {
		segment := f.segments[index]
		return segment.start+segment.size > offset
	})
	if index == len(f.segments) {
		return -1
	}
	return index
}

func (f *compositeFileStream) openSegment(index int, segment compositeSegment) error {
	if f.current != nil && f.currentIndex == index {
		return nil
	}
	if err := f.closeCurrent(); err != nil {
		return err
	}
	reader, err := f.manager.lowlevelIOStream(
		f.manager.bkio,
		segment.sourceFileID,
		segment.size,
	)(f.ctx)
	if err != nil {
		return fmt.Errorf("open composite source %d: %w", segment.sourceFileID, err)
	}
	position := f.offset - segment.start
	if position != 0 {
		if _, err := reader.Seek(position, io.SeekStart); err != nil {
			_ = reader.Close()
			return fmt.Errorf("seek composite source %d: %w", segment.sourceFileID, err)
		}
	}
	f.current = reader
	f.currentIndex = index
	return nil
}

func (f *compositeFileStream) Seek(offset int64, whence int) (int64, error) {
	if !f.open {
		return 0, ErrFileNotOpen
	}
	next := f.offset
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next += offset
	case io.SeekEnd:
		next = f.size + offset
	default:
		return f.offset, fmt.Errorf("%w: whence=%d", ErrInvalidOffset, whence)
	}
	if next < 0 {
		return f.offset, fmt.Errorf("%w: %d", ErrInvalidOffset, next)
	}
	if next > f.size {
		return f.size, fmt.Errorf("%w: offset=%d size=%d", ErrSeekPastEnd, next, f.size)
	}
	if err := f.closeCurrent(); err != nil {
		return f.offset, err
	}
	f.offset = next
	return next, nil
}

func (f *compositeFileStream) Close() error {
	if !f.open {
		return nil
	}
	f.open = false
	return f.closeCurrent()
}

func (f *compositeFileStream) closeCurrent() error {
	if f.current == nil {
		f.currentIndex = -1
		return nil
	}
	err := f.current.Close()
	f.current = nil
	f.currentIndex = -1
	if err != nil {
		return fmt.Errorf("close composite source: %w", err)
	}
	return nil
}
