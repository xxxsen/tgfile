package model

import "encoding/xml"

// Multistatus represents the root element in a WebDAV PROPFIND response
type Multistatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	Responses []Response `xml:"D:response"`
}

// Response represents an individual resource in the WebDAV response
type Response struct {
	Href      string     `xml:"D:href"`
	Propstats []Propstat `xml:"D:propstat"`
}

// Propstat represents the property and status of a resource
type Propstat struct {
	Prop   Prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

// Prop represents the properties of a resource
type Prop struct {
	DisplayName   string       `xml:"D:displayname,omitempty"`
	ContentLength string       `xml:"D:getcontentlength,omitempty"`
	ResourceType  ResourceType `xml:"D:resourcetype,omitempty"`
}

// ResourceType represents whether the resource is a collection or not
type ResourceType struct {
	Collection string `xml:"D:collection,omitempty"`
}
