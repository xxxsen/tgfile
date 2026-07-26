package webdav

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/filemgr"
)

const (
	defaultLockTimeout = time.Hour
	maxLockTimeout     = 24 * time.Hour
)

func (h *WebdavHandler) handleLock(c *gin.Context) {
	raw, err := readLimitedXMLBody(c.Request)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	timeout, err := parseLockTimeout(c.GetHeader("Timeout"))
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		h.handleLockRefresh(c, timeout)
		return
	}
	depth := c.GetHeader("Depth")
	if depth == "" {
		depth = "infinity"
	}
	if depth != "0" && depth != "infinity" {
		h.writeMappedError(c, errInvalidDepth)
		return
	}
	owner, err := parseLockInfo(raw)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	ifHeader, err := h.parseIfHeader(c, c.GetHeader("If"))
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	result, err := h.fmgr.LockWebDAVResource(
		c.Request.Context(),
		filemgr.WebDAVLockRequest{
			Path:       h.buildSrcPath(c),
			Depth:      depth,
			OwnerXML:   owner,
			Principal:  h.principal(c),
			Timeout:    timeout,
			IfHeader:   ifHeader,
			MaxEntries: h.maxMutationEntries,
		},
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	c.Header("Lock-Token", "<"+result.Lock.Token+">")
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	h.writeLockResponse(c, status, result.Lock)
}

func (h *WebdavHandler) handleLockRefresh(c *gin.Context, timeout time.Duration) {
	ifHeader, err := h.parseIfHeader(c, c.GetHeader("If"))
	if err != nil || ifHeader == nil {
		h.writeMappedError(c, errInvalidIfHeader)
		return
	}
	token, ok := singlePositiveLockToken(ifHeader)
	if !ok {
		h.writeMappedError(c, errInvalidIfHeader)
		return
	}
	lock, err := h.fmgr.RefreshWebDAVLock(
		c.Request.Context(),
		h.buildSrcPath(c),
		token,
		h.principal(c),
		timeout,
		ifHeader,
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	c.Header("Lock-Token", "<"+lock.Token+">")
	h.writeLockResponse(c, http.StatusOK, *lock)
}

func (h *WebdavHandler) handleUnlock(c *gin.Context) {
	token, err := parseLockTokenHeader(c.GetHeader("Lock-Token"))
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	if err := h.fmgr.UnlockWebDAVResource(
		c.Request.Context(),
		h.buildSrcPath(c),
		token,
		h.principal(c),
	); err != nil {
		h.writeMappedError(c, err)
		return
	}
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusNoContent)
}

func (h *WebdavHandler) writeLockResponse(
	c *gin.Context,
	status int,
	lock filemgr.WebDAVLock,
) {
	c.Header("Content-Type", "application/xml; charset=utf-8")
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(status)
	encoder := xml.NewEncoder(c.Writer)
	lock.RootPath = h.externalPath(lock.RootPath, false)
	property := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "prop"}}
	_ = encoder.EncodeToken(property)
	_ = encodeDAVProperty(encoder, davPropertyValue{
		Name: filemgr.WebDAVPropertyName{
			Namespace: davNamespace,
			LocalName: "lockdiscovery",
		},
		Kind:  "lockdiscovery",
		Locks: []filemgr.WebDAVLock{lock},
	})
	_ = encoder.EncodeToken(property.End())
	_ = encoder.Flush()
}

func parseLockTimeout(value string) (time.Duration, error) {
	if value == "" {
		return defaultLockTimeout, nil
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if strings.EqualFold(candidate, "Infinite") {
			return maxLockTimeout, nil
		}
		if !strings.HasPrefix(candidate, "Second-") {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimPrefix(candidate, "Second-"), 10, 64)
		if err != nil || seconds <= 0 {
			continue
		}
		if seconds >= int64(maxLockTimeout/time.Second) {
			return maxLockTimeout, nil
		}
		return time.Duration(seconds) * time.Second, nil
	}
	return 0, errUnsupportedLockTimeout
}

func parseLockInfo(raw []byte) (string, error) {
	var info struct {
		XMLName xml.Name
		Scope   struct {
			Exclusive *struct{} `xml:"DAV: exclusive"`
		} `xml:"DAV: lockscope"`
		Type struct {
			Write *struct{} `xml:"DAV: write"`
		} `xml:"DAV: locktype"`
		Owner struct {
			InnerXML string `xml:",innerxml"`
		} `xml:"DAV: owner"`
	}
	if err := xml.Unmarshal(raw, &info); err != nil ||
		info.XMLName.Space != davNamespace ||
		info.XMLName.Local != "lockinfo" {
		return "", errInvalidLockInfo
	}
	if info.Scope.Exclusive == nil || info.Type.Write == nil {
		return "", errUnsupportedLockScope
	}
	return info.Owner.InnerXML, nil
}

func parseLockTokenHeader(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || value[0] != '<' || value[len(value)-1] != '>' {
		return "", errInvalidLockToken
	}
	token := value[1 : len(value)-1]
	if !strings.HasPrefix(token, "opaquelocktoken:") {
		return "", errInvalidLockToken
	}
	return token, nil
}

func singlePositiveLockToken(header *filemgr.WebDAVIfHeader) (string, bool) {
	var token string
	for _, list := range header.Lists {
		for _, term := range list.Terms {
			if term.Not || term.LockToken == "" {
				continue
			}
			if token != "" && token != term.LockToken {
				return "", false
			}
			token = term.LockToken
		}
	}
	return token, token != ""
}
