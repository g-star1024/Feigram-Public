package main

import (
	"reflect"
	"testing"
)

func TestDialogFolderIDs(t *testing.T) {
	got := dialogFolderIDs(false)
	if !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("不显示归档时只应拉 folder 0，got=%v", got)
	}

	got = dialogFolderIDs(true)
	if !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("显示归档时应拉 folder 0 和 1，got=%v", got)
	}
}
