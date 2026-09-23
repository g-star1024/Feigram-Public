package main

// R4.19：下载目录权限错误要带可操作提示（2.5.3 实测反馈——
// 用户把下载目录指到 /vol2/1000/movie/feigrampub，应用用户无写权限，
// 裸报 "open ...: permission denied" 无从下手）。

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestStorageErrorHint(t *testing.T) {
	dir := "/vol2/1000/movie/feigrampub"

	// 非权限错误：原样返回，不加提示。
	plain := fmt.Errorf("open /x/y.mp4.part: connection reset")
	if got := storageErrorHint(plain, dir); !errors.Is(got, plain) || got != plain {
		t.Fatalf("非权限错误应原样返回，got %v", got)
	}

	// 权限错误：追加可操作提示与目录。
	perm := &fs.PathError{Op: "open", Path: "/x/y.mp4.part", Err: fs.ErrPermission}
	got := storageErrorHint(perm, dir)
	if !errors.Is(got, fs.ErrPermission) {
		t.Fatalf("应保留 fs.ErrPermission 语义，got %v", got)
	}
	if !strings.Contains(got.Error(), "权限不足") || !strings.Contains(got.Error(), dir) {
		t.Fatalf("权限错误应带可操作提示与目录，got %v", got)
	}

	// nil 透传。
	if storageErrorHint(nil, dir) != nil {
		t.Fatalf("nil 应返回 nil")
	}
}
