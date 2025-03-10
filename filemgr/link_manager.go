package filemgr

import (
	"context"
	"fmt"
	"tgfile/service"
)

var defaultLinkMgr ILinkManager

type IterLinkFunc func(ctx context.Context, link string, fileid uint64) (bool, error)

type ILinkManager interface {
	CreateLink(ctx context.Context, link string, fileid uint64) error
	ResolveLink(ctx context.Context, link string) (uint64, error)
	IterLink(ctx context.Context, cb IterLinkFunc) error
}

func SetLinkManagerImpl(mgr ILinkManager) {
	defaultLinkMgr = mgr
}

func CreateLink(ctx context.Context, link string, fileid uint64) error {
	return defaultLinkMgr.CreateLink(ctx, link, fileid)
}

func ResolveLink(ctx context.Context, link string) (uint64, error) {
	return defaultLinkMgr.ResolveLink(ctx, link)
}

func IterLink(ctx context.Context, cb IterLinkFunc) error {
	return defaultLinkMgr.IterLink(ctx, cb)
}

type defaultLinkManager struct {
	fmgr IFileManager
}

func (d *defaultLinkManager) CreateLink(ctx context.Context, link string, fileid uint64) error {
	if err := service.FileMappingService.CreateFileMapping(ctx, link, fileid); err != nil {
		return err
	}
	return nil
}

func (d *defaultLinkManager) ResolveLink(ctx context.Context, link string) (uint64, error) {
	fid, ok, err := service.FileMappingService.GetFileMapping(ctx, link)
	if err != nil {
		return 0, fmt.Errorf("open mapping failed, err:%w", err)
	}
	if !ok {
		return 0, fmt.Errorf("link not found")
	}
	return fid, nil
}

func NewLinkManager(fmgr IFileManager) ILinkManager {
	return &defaultLinkManager{
		fmgr: fmgr,
	}
}

func (d *defaultLinkManager) IterLink(ctx context.Context, cb IterLinkFunc) error {
	return service.FileMappingService.IterFileMapping(ctx, func(ctx context.Context, filename string, fileid uint64) (bool, error) {
		return cb(ctx, filename, fileid)
	})
}
