package main

import "testing"

// R4.55①：缓存过滤层的纯函数测试。
func TestFilterCachedDialogs(t *testing.T) {
	items := []map[string]any{
		{"id": "u1", "archived": false},
		{"id": "u2", "archived": true},
		{"id": "u3", "archived": false},
	}

	got, err := filterCachedDialogs(items, nil, 0, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 || got[0]["id"] != "u1" || got[1]["id"] != "u3" {
		t.Fatalf("includeArchived=false 应过滤归档项，got %v", got)
	}

	got, err = filterCachedDialogs(items, nil, 0, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("includeArchived=true 应保留全部，got %d", len(got))
	}

	// limit 截断
	got, _ = filterCachedDialogs(items, nil, 1, false)
	if len(got) != 1 || got[0]["id"] != "u1" {
		t.Fatalf("limit=1 应只返回第一条非归档，got %v", got)
	}

	// 错误透传
	if _, err = filterCachedDialogs(items, errTestBoom, 0, true); err == nil {
		t.Fatal("err 应原样透传")
	}
}

var errTestBoom = &simpleTestError{}

type simpleTestError struct{}

func (*simpleTestError) Error() string { return "boom" }
