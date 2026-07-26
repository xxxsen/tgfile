package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
)

type syncCollectionRequest struct {
	Token      string
	Level      string
	Properties []filemgr.WebDAVPropertyName
}

func (h *WebdavHandler) handleReport(c *gin.Context) {
	if c.GetHeader("Depth") != "0" {
		h.writeMappedError(c, errInvalidDepth)
		return
	}
	request, err := parseSyncCollectionRequest(c.Request)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	since, err := parseSyncToken(request.Token)
	if err != nil {
		h.writeError(c, http.StatusForbidden, err, "valid-sync-token")
		return
	}
	if request.Token == "" {
		since = -1
	}
	rootPath := h.buildSrcPath(c)
	rootItem, err := h.fmgr.StatFileLink(c.Request.Context(), rootPath)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	if !rootItem.IsDir {
		h.writeError(c, http.StatusForbidden, errSyncRootNotCollection, "")
		return
	}
	page, err := h.fmgr.WebDAVChanges(
		c.Request.Context(),
		rootPath,
		since,
		request.Level,
		h.syncPageSize,
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}

	c.Header("Content-Type", "application/xml; charset=utf-8")
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusMultiStatus)
	encoder := xml.NewEncoder(c.Writer)
	multistatus := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "multistatus"}}
	if err := encoder.EncodeToken(multistatus); err != nil {
		return
	}
	spec := &propertyFindRequest{
		Mode:       propertyExplicit,
		Properties: request.Properties,
	}
	if len(spec.Properties) == 0 {
		spec.Properties = []filemgr.WebDAVPropertyName{
			{Namespace: davNamespace, LocalName: "getetag"},
			{Namespace: davNamespace, LocalName: "resourcetype"},
		}
	}
	if err := h.writeSyncResponses(
		c.Request.Context(),
		encoder,
		rootPath,
		request.Token,
		request.Level,
		page.Changes,
		spec,
	); err != nil {
		return
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "sync-token"},
		formatSyncToken(page.SyncRevision),
	); err != nil {
		return
	}
	if err := encoder.EncodeToken(multistatus.End()); err != nil {
		return
	}
	_ = encoder.Flush()
}

func (h *WebdavHandler) writeSyncResponses(
	ctx context.Context,
	encoder *xml.Encoder,
	rootPath, token, level string,
	changes []filemgr.WebDAVChange,
	spec *propertyFindRequest,
) error {
	if token == "" {
		if err := h.writeInitialSyncResponses(ctx, encoder, rootPath, level, spec); err != nil {
			return fmt.Errorf("walk initial WebDAV sync collection: %w", err)
		}
		return nil
	}
	for _, change := range changes {
		if err := h.writeSyncChange(ctx, encoder, change, spec); err != nil {
			return err
		}
	}
	return nil
}

func (h *WebdavHandler) writeInitialSyncResponses(
	ctx context.Context,
	encoder *xml.Encoder,
	rootPath, level string,
	spec *propertyFindRequest,
) error {
	err := h.fmgr.WalkFileLink(
		ctx,
		rootPath,
		func(
			ctx context.Context,
			childPath string,
			item *entity.FileLinkMeta,
		) (bool, error) {
			if err := h.writePropertyResponse(ctx, encoder, childPath, item, spec); err != nil {
				return false, err
			}
			if level != "infinity" || !item.IsDir {
				return true, nil
			}
			if err := h.writeInitialSyncResponses(ctx, encoder, childPath, level, spec); err != nil {
				return false, err
			}
			return true, nil
		},
	)
	if err != nil {
		return fmt.Errorf("walk initial sync path %q: %w", rootPath, err)
	}
	return nil
}

func (h *WebdavHandler) writeSyncChange(
	ctx context.Context,
	encoder *xml.Encoder,
	change filemgr.WebDAVChange,
	spec *propertyFindRequest,
) error {
	item, err := h.fmgr.StatFileLink(ctx, change.Path)
	if err == nil {
		return h.writePropertyResponse(ctx, encoder, change.Path, item, spec)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat changed WebDAV resource: %w", err)
	}
	return h.writeDAVStatusResponse(
		encoder,
		h.externalPath(change.Path, false),
		http.StatusNotFound,
	)
}

func parseSyncCollectionRequest(request *http.Request) (*syncCollectionRequest, error) {
	raw, err := readLimitedXMLBody(request)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errReportBodyRequired
	}
	var envelope struct {
		XMLName xml.Name
		Token   []string                `xml:"DAV: sync-token"`
		Level   []string                `xml:"DAV: sync-level"`
		Prop    []propertyNameContainer `xml:"DAV: prop"`
		Unknown []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnsupportedReport, err)
	}
	if envelope.XMLName.Space != davNamespace ||
		envelope.XMLName.Local != "sync-collection" {
		return nil, errUnsupportedReport
	}
	if len(envelope.Unknown) != 0 ||
		len(envelope.Token) > 1 ||
		len(envelope.Level) > 1 ||
		len(envelope.Prop) > 1 {
		return nil, errInvalidSyncInstruction
	}
	result := &syncCollectionRequest{Level: "1"}
	if len(envelope.Token) == 1 {
		result.Token = strings.TrimSpace(envelope.Token[0])
	}
	if len(envelope.Prop) == 1 {
		result.Properties = envelope.Prop[0].Names
	}
	if len(envelope.Level) == 0 {
		return result, nil
	}
	level := strings.TrimSpace(envelope.Level[0])
	switch level {
	case "1":
		result.Level = level
	case "infinite":
		result.Level = "infinity"
	default:
		return nil, errUnsupportedSyncLevel
	}
	return result, nil
}

func (h *WebdavHandler) writeDAVStatusResponse(
	encoder *xml.Encoder,
	href string,
	status int,
) error {
	response := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "response"}}
	if err := encoder.EncodeToken(response); err != nil {
		return err
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "href"},
		href,
	); err != nil {
		return err
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "status"},
		fmt.Sprintf("HTTP/1.1 %d %s", status, http.StatusText(status)),
	); err != nil {
		return err
	}
	return encoder.EncodeToken(response.End())
}
