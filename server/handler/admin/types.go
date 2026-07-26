package admin

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/filemgr"
)

const (
	roleRead        = "read"
	roleReadWrite   = "read-write"
	maxAdminOrigins = 32
)

var (
	errInvalidAdminOptions    = errors.New("invalid admin handler options")
	errInsecureAdminOrigin    = errors.New("admin HTTP origin must be loopback")
	errInvalidAdminUsers      = errors.New("invalid admin users or roles")
	errInvalidAdminOrigin     = errors.New("invalid admin origin")
	errInvalidIntegerParam    = errors.New("invalid integer parameter")
	errInvalidBooleanParam    = errors.New("invalid boolean parameter")
	errInvalidEntryCursor     = errors.New("invalid entry cursor")
	errInvalidBackupJobCursor = errors.New("invalid backup job cursor")
)

type Options struct {
	FileManager      filemgr.IFileManager
	BackupManager    *backupmgr.Manager
	Users            map[string]string
	Roles            map[string]string
	ExternalOrigins  []string
	SessionIdle      time.Duration
	SessionMaximum   time.Duration
	MaxUploadSize    int64
	MaxPathBytes     int
	MutationMaxItems int
}

type Handler struct {
	files            filemgr.IFileManager
	backups          *backupmgr.Manager
	users            map[string]string
	roles            map[string]string
	externalOrigins  map[string]struct{}
	secureCookie     bool
	maxUploadSize    int64
	maxPathBytes     int
	mutationMaxItems int
	sessions         *sessionStore
	loginLimiter     *loginLimiter
	dummyPassword    [sha256.Size]byte
	api              http.Handler
}

type principal struct {
	Username string
	Role     string
	CSRF     string
}

type responseEnvelope struct {
	Data any `json:"data"`
}

type errorEnvelope struct {
	Error publicError `json:"error"`
}

type publicError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type entryDTO struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Ctime int64  `json:"ctime"`
	Mtime int64  `json:"mtime"`
	ETag  string `json:"etag"`
}
