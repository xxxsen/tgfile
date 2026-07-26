package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/httpkit"
)

type propertySelectionMode int

const (
	propertyAll propertySelectionMode = iota
	propertyNames
	propertyExplicit
)

type propertyFindRequest struct {
	Mode       propertySelectionMode
	Properties []filemgr.WebDAVPropertyName
	Include    []filemgr.WebDAVPropertyName
}

type propertyNameContainer struct {
	Names []filemgr.WebDAVPropertyName
}

func (p *propertyNameContainer) UnmarshalXML(
	decoder *xml.Decoder,
	start xml.StartElement,
) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode WebDAV property names: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			p.Names = append(p.Names, filemgr.WebDAVPropertyName{
				Namespace: value.Name.Space,
				LocalName: value.Name.Local,
			})
			if err := decoder.Skip(); err != nil {
				return fmt.Errorf("skip WebDAV property value: %w", err)
			}
		case xml.EndElement:
			if value.Name == start.Name {
				p.Names = deduplicatePropertyNames(p.Names)
				return nil
			}
		}
	}
}

type davPropertyValue struct {
	Name       filemgr.WebDAVPropertyName
	Text       string
	InnerXML   string
	Collection bool
	Locks      []filemgr.WebDAVLock
	Kind       string
}

type davPropstat struct {
	Status     int
	Properties []davPropertyValue
}

var coreLivePropertyNames = []filemgr.WebDAVPropertyName{
	{Namespace: davNamespace, LocalName: "displayname"},
	{Namespace: davNamespace, LocalName: "creationdate"},
	{Namespace: davNamespace, LocalName: "getlastmodified"},
	{Namespace: davNamespace, LocalName: "getcontentlength"},
	{Namespace: davNamespace, LocalName: "getcontenttype"},
	{Namespace: davNamespace, LocalName: "getetag"},
	{Namespace: davNamespace, LocalName: "resourcetype"},
	{Namespace: davNamespace, LocalName: "supportedlock"},
	{Namespace: davNamespace, LocalName: "lockdiscovery"},
	{Namespace: davNamespace, LocalName: "supported-report-set"},
	{Namespace: davNamespace, LocalName: "sync-token"},
}

func (h *WebdavHandler) handlePropfind(c *gin.Context) {
	depth, err := parsePropfindDepth(c.GetHeader("Depth"))
	if errors.Is(err, errInfinitePropfind) {
		h.writeError(c, http.StatusForbidden, err, "propfind-finite-depth")
		return
	}
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	spec, err := parsePropfindRequest(c.Request)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	location := h.buildSrcPath(c)
	base, err := h.fmgr.StatFileLink(c.Request.Context(), location)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}

	c.Header("Content-Type", "application/xml; charset=utf-8")
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusMultiStatus)
	encoder := xml.NewEncoder(c.Writer)
	root := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "multistatus"}}
	if err := encoder.EncodeToken(root); err != nil {
		return
	}
	if err := h.writePropertyResponse(
		c.Request.Context(),
		encoder,
		location,
		base,
		spec,
	); err != nil {
		return
	}
	if depth == 1 && base.IsDir {
		err = h.fmgr.WalkFileLink(
			c.Request.Context(),
			location,
			func(
				ctx context.Context,
				childPath string,
				item *entity.FileLinkMeta,
			) (bool, error) {
				if err := h.writePropertyResponse(ctx, encoder, childPath, item, spec); err != nil {
					return false, err
				}
				return true, nil
			},
		)
		if err != nil {
			return
		}
	}
	_ = encoder.EncodeToken(root.End())
	_ = encoder.Flush()
}

func parsePropfindDepth(value string) (int, error) {
	switch value {
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	case "", "infinity":
		return 0, errInfinitePropfind
	default:
		return 0, errInvalidDepth
	}
}

func parsePropfindRequest(request *http.Request) (*propertyFindRequest, error) {
	raw, err := readLimitedXMLBody(request)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return &propertyFindRequest{Mode: propertyAll}, nil
	}
	var envelope struct {
		XMLName  xml.Name
		AllProp  []struct{}              `xml:"DAV: allprop"`
		PropName []struct{}              `xml:"DAV: propname"`
		Prop     []propertyNameContainer `xml:"DAV: prop"`
		Include  []propertyNameContainer `xml:"DAV: include"`
		Unknown  []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalidPropfind, err)
	}
	if envelope.XMLName.Space != davNamespace || envelope.XMLName.Local != "propfind" {
		return nil, errInvalidPropfind
	}
	if len(envelope.Unknown) != 0 {
		return nil, errInvalidPropfindElement
	}
	modeCount := len(envelope.AllProp) + len(envelope.PropName) + len(envelope.Prop)
	if modeCount == 0 {
		return nil, errMissingPropfindMode
	}
	if modeCount != 1 || len(envelope.Include) > 1 {
		return nil, errMultiplePropfindModes
	}
	result := &propertyFindRequest{Mode: propertyAll}
	switch {
	case len(envelope.PropName) == 1:
		result.Mode = propertyNames
	case len(envelope.Prop) == 1:
		result.Mode = propertyExplicit
		result.Properties = envelope.Prop[0].Names
	}
	if len(envelope.Include) == 1 {
		if result.Mode != propertyAll {
			return nil, errInvalidPropfindInclude
		}
		result.Include = envelope.Include[0].Names
	}
	return result, nil
}

func deduplicatePropertyNames(
	names []filemgr.WebDAVPropertyName,
) []filemgr.WebDAVPropertyName {
	seen := make(map[filemgr.WebDAVPropertyName]struct{}, len(names))
	result := make([]filemgr.WebDAVPropertyName, 0, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

func nextStartElement(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		if start, ok := token.(xml.StartElement); ok {
			return start, nil
		}
	}
}

func (h *WebdavHandler) writePropertyResponse(
	ctx context.Context,
	encoder *xml.Encoder,
	resourcePath string,
	item *entity.FileLinkMeta,
	spec *propertyFindRequest,
) error {
	dead, err := h.fmgr.ReadWebDAVProperties(ctx, item.EntryID)
	if err != nil {
		return fmt.Errorf("read WebDAV properties: %w", err)
	}
	locks, err := h.fmgr.ListWebDAVLocks(ctx, resourcePath)
	if err != nil {
		return fmt.Errorf("list WebDAV locks: %w", err)
	}
	requested := requestedPropertyNames(spec, dead, h.quotaBytes > 0)
	okProperties := make([]davPropertyValue, 0, len(requested))
	missingProperties := make([]davPropertyValue, 0)
	for _, name := range requested {
		value, found, err := h.resolveDAVProperty(ctx, resourcePath, item, name, dead, locks)
		if err != nil {
			return err
		}
		if spec.Mode == propertyNames {
			value = davPropertyValue{Name: name}
			found = true
		}
		if found {
			okProperties = append(okProperties, value)
		} else {
			missingProperties = append(missingProperties, davPropertyValue{Name: name})
		}
	}
	propstats := make([]davPropstat, 0, 2)
	if len(okProperties) != 0 {
		propstats = append(propstats, davPropstat{
			Status:     http.StatusOK,
			Properties: okProperties,
		})
	}
	if len(missingProperties) != 0 {
		propstats = append(propstats, davPropstat{
			Status:     http.StatusNotFound,
			Properties: missingProperties,
		})
	}
	return h.writeDAVResponseElement(
		encoder,
		h.externalPath(resourcePath, item.IsDir),
		propstats,
	)
}

func requestedPropertyNames(
	spec *propertyFindRequest,
	dead []filemgr.WebDAVProperty,
	quotaEnabled bool,
) []filemgr.WebDAVPropertyName {
	if spec.Mode == propertyExplicit {
		return spec.Properties
	}
	names := append([]filemgr.WebDAVPropertyName(nil), coreLivePropertyNames...)
	if quotaEnabled {
		names = append(names,
			filemgr.WebDAVPropertyName{
				Namespace: davNamespace,
				LocalName: "quota-used-bytes",
			},
			filemgr.WebDAVPropertyName{
				Namespace: davNamespace,
				LocalName: "quota-available-bytes",
			},
		)
	}
	for _, property := range dead {
		names = append(names, property.Name)
	}
	names = append(names, spec.Include...)
	return deduplicatePropertyNames(names)
}

func (h *WebdavHandler) resolveDAVProperty(
	ctx context.Context,
	resourcePath string,
	item *entity.FileLinkMeta,
	name filemgr.WebDAVPropertyName,
	dead []filemgr.WebDAVProperty,
	locks []filemgr.WebDAVLock,
) (davPropertyValue, bool, error) {
	value := davPropertyValue{Name: name}
	if name.Namespace != davNamespace {
		return resolveDeadDAVProperty(value, dead)
	}
	return h.resolveLiveDAVProperty(ctx, resourcePath, item, value, dead, locks)
}

func (h *WebdavHandler) resolveLiveDAVProperty(
	ctx context.Context,
	resourcePath string,
	item *entity.FileLinkMeta,
	value davPropertyValue,
	dead []filemgr.WebDAVProperty,
	locks []filemgr.WebDAVLock,
) (davPropertyValue, bool, error) {
	switch value.Name.LocalName {
	case "getcontentlength", "getcontenttype", "getetag":
		return resolveFileDAVProperty(item, value)
	case "quota-used-bytes", "quota-available-bytes":
		return h.resolveQuotaDAVProperty(ctx, value)
	case "sync-token":
		return h.resolveSyncTokenDAVProperty(ctx, resourcePath, item, value)
	}
	switch value.Name.LocalName {
	case "displayname":
		value.Text = item.FileName
	case "creationdate":
		value.Text = time.UnixMilli(item.Ctime).UTC().Format(time.RFC3339)
	case "getlastmodified":
		value.Text = time.UnixMilli(item.Mtime).UTC().Format(http.TimeFormat)
	case "resourcetype":
		value.Collection = item.IsDir
		value.Kind = "resourcetype"
	case "supportedlock":
		value.Kind = "supportedlock"
	case "lockdiscovery":
		value.Kind = "lockdiscovery"
		value.Locks = h.externalizeLocks(locks)
	case "supported-report-set":
		value.Kind = "supported-report-set"
	default:
		return resolveDeadDAVProperty(value, dead)
	}
	return value, true, nil
}

func resolveFileDAVProperty(
	item *entity.FileLinkMeta,
	value davPropertyValue,
) (davPropertyValue, bool, error) {
	if item.IsDir {
		return value, false, nil
	}
	switch value.Name.LocalName {
	case "getcontentlength":
		value.Text = strconv.FormatInt(item.FileSize, 10)
	case "getcontenttype":
		value.Text = httpkit.DetermineMimeType(item.FileName)
	case "getetag":
		value.Text = filemgr.WebDAVETag(item)
	}
	return value, true, nil
}

func (h *WebdavHandler) resolveQuotaDAVProperty(
	ctx context.Context,
	value davPropertyValue,
) (davPropertyValue, bool, error) {
	if h.quotaBytes <= 0 {
		return value, false, nil
	}
	used, available, err := h.fmgr.WebDAVQuota(ctx, h.davRoot, h.quotaBytes)
	if err != nil {
		return value, false, fmt.Errorf("read WebDAV quota: %w", err)
	}
	if value.Name.LocalName == "quota-used-bytes" {
		value.Text = strconv.FormatInt(used, 10)
	} else {
		value.Text = strconv.FormatInt(available, 10)
	}
	return value, true, nil
}

func (h *WebdavHandler) resolveSyncTokenDAVProperty(
	ctx context.Context,
	resourcePath string,
	item *entity.FileLinkMeta,
	value davPropertyValue,
) (davPropertyValue, bool, error) {
	if !item.IsDir {
		return value, false, nil
	}
	page, err := h.fmgr.WebDAVChanges(ctx, resourcePath, -1, "0", 1)
	if err != nil {
		return value, false, fmt.Errorf("read WebDAV sync revision: %w", err)
	}
	value.Text = formatSyncToken(page.SyncRevision)
	return value, true, nil
}

func resolveDeadDAVProperty(
	value davPropertyValue,
	dead []filemgr.WebDAVProperty,
) (davPropertyValue, bool, error) {
	for _, property := range dead {
		if property.Name == value.Name {
			value.InnerXML = property.ValueXML
			return value, true, nil
		}
	}
	return value, false, nil
}

func (h *WebdavHandler) externalizeLocks(locks []filemgr.WebDAVLock) []filemgr.WebDAVLock {
	result := make([]filemgr.WebDAVLock, len(locks))
	copy(result, locks)
	for index := range result {
		result[index].RootPath = h.externalPath(result[index].RootPath, false)
	}
	return result
}

func (h *WebdavHandler) writeDAVResponseElement(
	encoder *xml.Encoder,
	href string,
	propstats []davPropstat,
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
	for _, propstat := range propstats {
		start := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "propstat"}}
		if err := encoder.EncodeToken(start); err != nil {
			return err
		}
		properties := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "prop"}}
		if err := encoder.EncodeToken(properties); err != nil {
			return err
		}
		for _, property := range propstat.Properties {
			if err := encodeDAVProperty(encoder, property); err != nil {
				return err
			}
		}
		if err := encoder.EncodeToken(properties.End()); err != nil {
			return err
		}
		statusText := fmt.Sprintf(
			"HTTP/1.1 %d %s",
			propstat.Status,
			http.StatusText(propstat.Status),
		)
		if err := encodeSimpleElement(
			encoder,
			xml.Name{Space: davNamespace, Local: "status"},
			statusText,
		); err != nil {
			return err
		}
		if err := encoder.EncodeToken(start.End()); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(response.End())
}

func encodeDAVProperty(encoder *xml.Encoder, property davPropertyValue) error {
	start := xml.StartElement{Name: xml.Name{
		Space: property.Name.Namespace,
		Local: property.Name.LocalName,
	}}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if err := encodeDAVPropertyValue(encoder, property); err != nil {
		return err
	}
	return encoder.EncodeToken(start.End())
}

func encodeDAVPropertyValue(encoder *xml.Encoder, property davPropertyValue) error {
	switch property.Kind {
	case "resourcetype":
		if property.Collection {
			if err := encodeEmptyElement(
				encoder,
				xml.Name{Space: davNamespace, Local: "collection"},
			); err != nil {
				return err
			}
		}
	case "supportedlock":
		if err := encodeSupportedLock(encoder); err != nil {
			return err
		}
	case "lockdiscovery":
		for _, lock := range property.Locks {
			if err := encodeActiveLock(encoder, lock); err != nil {
				return err
			}
		}
	case "supported-report-set":
		if err := encodeSupportedReportSet(encoder); err != nil {
			return err
		}
	default:
		if property.InnerXML != "" {
			if err := encodeInnerXML(encoder, property.InnerXML); err != nil {
				return err
			}
		} else if property.Text != "" {
			if err := encoder.EncodeToken(xml.CharData(property.Text)); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeSimpleElement(
	encoder *xml.Encoder,
	name xml.Name,
	value string,
) error {
	start := xml.StartElement{Name: name}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if err := encoder.EncodeToken(xml.CharData(value)); err != nil {
		return err
	}
	return encoder.EncodeToken(start.End())
}

func encodeEmptyElement(encoder *xml.Encoder, name xml.Name) error {
	start := xml.StartElement{Name: name}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	return encoder.EncodeToken(start.End())
}

func encodeInnerXML(encoder *xml.Encoder, inner string) error {
	decoder := xml.NewDecoder(strings.NewReader("<wrapper>" + inner + "</wrapper>"))
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode stored WebDAV property: %w", err)
		}
		switch token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				continue
			}
		case xml.EndElement:
			if depth == 1 {
				depth--
				continue
			}
			depth--
		}
		token = stripXMLNamespaceAttributes(token)
		if err := encoder.EncodeToken(token); err != nil {
			return err
		}
	}
}

func stripXMLNamespaceAttributes(token xml.Token) xml.Token {
	start, ok := token.(xml.StartElement)
	if !ok {
		return token
	}
	attributes := start.Attr[:0]
	for _, attribute := range start.Attr {
		if attribute.Name.Space == "xmlns" ||
			(attribute.Name.Space == "" && attribute.Name.Local == "xmlns") {
			continue
		}
		attributes = append(attributes, attribute)
	}
	start.Attr = attributes
	return start
}

func encodeSupportedLock(encoder *xml.Encoder) error {
	entry := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "lockentry"}}
	if err := encoder.EncodeToken(entry); err != nil {
		return err
	}
	scope := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "lockscope"}}
	if err := encoder.EncodeToken(scope); err != nil {
		return err
	}
	if err := encodeEmptyElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "exclusive"},
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(scope.End()); err != nil {
		return err
	}
	lockType := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "locktype"}}
	if err := encoder.EncodeToken(lockType); err != nil {
		return err
	}
	if err := encodeEmptyElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "write"},
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(lockType.End()); err != nil {
		return err
	}
	return encoder.EncodeToken(entry.End())
}

func encodeActiveLock(encoder *xml.Encoder, lock filemgr.WebDAVLock) error {
	active := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "activelock"}}
	if err := encoder.EncodeToken(active); err != nil {
		return err
	}
	if err := encodeLockShape(encoder); err != nil {
		return err
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "depth"},
		lock.Depth,
	); err != nil {
		return err
	}
	if lock.OwnerXML != "" {
		owner := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "owner"}}
		if err := encoder.EncodeToken(owner); err != nil {
			return err
		}
		if err := encodeInnerXML(encoder, lock.OwnerXML); err != nil {
			return err
		}
		if err := encoder.EncodeToken(owner.End()); err != nil {
			return err
		}
	}
	seconds := max(0, (lock.ExpiresAt-time.Now().UnixMilli())/1000)
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "timeout"},
		fmt.Sprintf("Second-%d", seconds),
	); err != nil {
		return err
	}
	lockToken := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "locktoken"}}
	if err := encoder.EncodeToken(lockToken); err != nil {
		return err
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "href"},
		lock.Token,
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(lockToken.End()); err != nil {
		return err
	}
	lockRoot := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "lockroot"}}
	if err := encoder.EncodeToken(lockRoot); err != nil {
		return err
	}
	if err := encodeSimpleElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "href"},
		lock.RootPath,
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(lockRoot.End()); err != nil {
		return err
	}
	return encoder.EncodeToken(active.End())
}

func encodeLockShape(encoder *xml.Encoder) error {
	scope := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "lockscope"}}
	if err := encoder.EncodeToken(scope); err != nil {
		return err
	}
	if err := encodeEmptyElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "exclusive"},
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(scope.End()); err != nil {
		return err
	}
	lockType := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "locktype"}}
	if err := encoder.EncodeToken(lockType); err != nil {
		return err
	}
	if err := encodeEmptyElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "write"},
	); err != nil {
		return err
	}
	return encoder.EncodeToken(lockType.End())
}

func encodeSupportedReportSet(encoder *xml.Encoder) error {
	report := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "supported-report"}}
	if err := encoder.EncodeToken(report); err != nil {
		return err
	}
	reportBody := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "report"}}
	if err := encoder.EncodeToken(reportBody); err != nil {
		return err
	}
	if err := encodeEmptyElement(
		encoder,
		xml.Name{Space: davNamespace, Local: "sync-collection"},
	); err != nil {
		return err
	}
	if err := encoder.EncodeToken(reportBody.End()); err != nil {
		return err
	}
	return encoder.EncodeToken(report.End())
}

func (h *WebdavHandler) handlePropPatch(c *gin.Context) {
	patches, err := parsePropertyUpdate(c.Request)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	if _, ok := h.stat(c); !ok {
		return
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	hasProtectedProperty := false
	for _, patch := range patches {
		if patch.Property.Name.Namespace == davNamespace {
			hasProtectedProperty = true
			break
		}
	}
	statuses := make([]int, len(patches))
	if hasProtectedProperty {
		for index, patch := range patches {
			statuses[index] = http.StatusFailedDependency
			if patch.Property.Name.Namespace == davNamespace {
				statuses[index] = http.StatusForbidden
			}
		}
	} else {
		err = h.fmgr.PatchWebDAVProperties(
			c.Request.Context(),
			h.buildSrcPath(c),
			patches,
			h.mutationOptions(c, condition),
		)
		if err != nil {
			h.writeMappedError(c, err)
			return
		}
		for index := range statuses {
			statuses[index] = http.StatusOK
		}
	}
	grouped := make(map[int][]davPropertyValue)
	order := make([]int, 0, 2)
	for index, status := range statuses {
		if _, exists := grouped[status]; !exists {
			order = append(order, status)
		}
		grouped[status] = append(grouped[status], davPropertyValue{
			Name: patches[index].Property.Name,
		})
	}
	propstats := make([]davPropstat, 0, len(order))
	for _, status := range order {
		propstats = append(propstats, davPropstat{
			Status:     status,
			Properties: grouped[status],
		})
	}
	c.Header("Content-Type", "application/xml; charset=utf-8")
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusMultiStatus)
	encoder := xml.NewEncoder(c.Writer)
	root := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "multistatus"}}
	_ = encoder.EncodeToken(root)
	_ = h.writeDAVResponseElement(
		encoder,
		h.externalPath(h.buildSrcPath(c), false),
		propstats,
	)
	_ = encoder.EncodeToken(root.End())
	_ = encoder.Flush()
}

func parsePropertyUpdate(request *http.Request) ([]filemgr.WebDAVPropertyPatch, error) {
	raw, err := readLimitedXMLBody(request)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errPropPatchBodyRequired
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	root, err := nextStartElement(decoder)
	if err != nil || root.Name.Space != davNamespace || root.Name.Local != "propertyupdate" {
		return nil, errInvalidPropertyUpdate
	}
	return decodePropertyUpdate(decoder, root)
}

func decodePropertyUpdate(
	decoder *xml.Decoder,
	root xml.StartElement,
) ([]filemgr.WebDAVPropertyPatch, error) {
	patches := make([]filemgr.WebDAVPropertyPatch, 0)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse PROPPATCH body: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != davNamespace ||
				(value.Name.Local != "set" && value.Name.Local != "remove") {
				return nil, errInvalidPropPatchOp
			}
			operationPatches, err := decodePropertyPatchOperation(
				decoder,
				value,
				value.Name.Local == "set",
			)
			if err != nil {
				return nil, err
			}
			patches = append(patches, operationPatches...)
		case xml.EndElement:
			if value.Name == root.Name {
				if len(patches) == 0 {
					return nil, errEmptyPropPatch
				}
				return patches, nil
			}
		}
	}
}

func decodePropertyPatchOperation(
	decoder *xml.Decoder,
	operation xml.StartElement,
	set bool,
) ([]filemgr.WebDAVPropertyPatch, error) {
	var patches []filemgr.WebDAVPropertyPatch
	propSeen := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != davNamespace || value.Name.Local != "prop" || propSeen {
				return nil, errInvalidPropPatchOp
			}
			propSeen = true
			properties, err := decodePatchProperties(decoder, value, set)
			if err != nil {
				return nil, err
			}
			patches = append(patches, properties...)
		case xml.EndElement:
			if value.Name == operation.Name {
				if !propSeen || len(patches) == 0 {
					return nil, errEmptyPropPatch
				}
				return patches, nil
			}
		}
	}
}

func decodePatchProperties(
	decoder *xml.Decoder,
	container xml.StartElement,
	set bool,
) ([]filemgr.WebDAVPropertyPatch, error) {
	patches := make([]filemgr.WebDAVPropertyPatch, 0)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			inner, err := captureInnerXML(decoder)
			if err != nil {
				return nil, err
			}
			patches = append(patches, filemgr.WebDAVPropertyPatch{
				Set: set,
				Property: filemgr.WebDAVProperty{
					Name: filemgr.WebDAVPropertyName{
						Namespace: value.Name.Space,
						LocalName: value.Name.Local,
					},
					ValueXML: inner,
				},
			})
		case xml.EndElement:
			if value.Name == container.Name {
				return patches, nil
			}
		}
	}
}

func captureInnerXML(
	decoder *xml.Decoder,
) (string, error) {
	var buffer bytes.Buffer
	encoder := xml.NewEncoder(&buffer)
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch token.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 {
				continue
			}
		}
		token = stripXMLNamespaceAttributes(token)
		if err := encoder.EncodeToken(token); err != nil {
			return "", err
		}
	}
	if err := encoder.Flush(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}
