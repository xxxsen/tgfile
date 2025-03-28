package model

import "encoding/xml"

// Multistatus 是 WebDAV 返回的根结构
type Multistatus struct {
	XMLName   xml.Name    `xml:"D:multistatus"`
	XMLNS     string      `xml:"xmlns:D,attr"`
	Responses []*Response `xml:"D:response"`
}

// Response 代表每个文件或目录的信息
type Response struct {
	Href     string   `xml:"D:href"`
	Propstat Propstat `xml:"D:propstat"`
}

// Propstat 包含资源的属性和状态
type Propstat struct {
	Prop   Prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

// Prop 存储 WebDAV 资源的各种属性
type Prop struct {
	DisplayName   string       `xml:"D:displayname"`
	LastModified  string       `xml:"D:getlastmodified"`
	ContentLength int64        `xml:"D:getcontentlength,omitempty"`
	ContentType   string       `xml:"D:getcontenttype,omitempty"`
	ResourceType  ResourceType `xml:"D:resourcetype"`
}

// ResourceType 用于区分文件和目录
type ResourceType struct {
	Collection string `xml:"D:collection,omitempty"`
}
