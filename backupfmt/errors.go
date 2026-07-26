package backupfmt

import "errors"

var (
	ErrInvalidArchive = errors.New("invalid backup archive")
	ErrLimitExceeded  = errors.New("backup archive limit exceeded")
	ErrChecksum       = errors.New("backup archive checksum mismatch")
	errMalformedJSON  = errors.New("malformed JSON value")
)
