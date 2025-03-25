package dao

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type testSubPair struct {
	link string
	subs []string
}

func TestResolveSub(t *testing.T) {
	d := &fileMappingDao{}
	testList := []*testSubPair{
		{
			link: "/",
			subs: []string{"/"},
		},
		{
			link: "/test",
			subs: []string{
				"/",
			},
		},
		{
			link: "/a/b/c",
			subs: []string{
				"/",
				"/a/",
				"/a/b/",
			},
		},
	}
	for _, item := range testList {
		subs, err := d.resolveSubPaths(item.link)
		assert.NoError(t, err)
		assert.Equal(t, item.subs, subs)
	}
}
