package entity

import "testing"

func TestFileInfoItemToFileMetaNormalizesHistoricalEmptyFileMD5(t *testing.T) {
	for _, extinfo := range []string{"", "{}", `{"md5":""}`, "invalid"} {
		t.Run(extinfo, func(t *testing.T) {
			meta := (&FileInfoItem{FileSize: 0, Extinfo: extinfo}).ToFileMeta()
			if meta.Md5Sum != EmptyFileMD5Sum {
				t.Fatalf("MD5 = %q, want %q", meta.Md5Sum, EmptyFileMD5Sum)
			}
		})
	}
}

func TestFileInfoItemToFileMetaPreservesStoredMD5(t *testing.T) {
	meta := (&FileInfoItem{
		FileSize: 1,
		Extinfo:  `{"md5":"stored"}`,
	}).ToFileMeta()
	if meta.Md5Sum != "stored" {
		t.Fatalf("MD5 = %q, want stored", meta.Md5Sum)
	}
}

func TestFileInfoItemToFileMetaDoesNotInventNonEmptyMD5(t *testing.T) {
	meta := (&FileInfoItem{FileSize: 1, Extinfo: "{}"}).ToFileMeta()
	if meta.Md5Sum != "" {
		t.Fatalf("MD5 = %q, want empty", meta.Md5Sum)
	}
}
