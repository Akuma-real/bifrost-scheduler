// package main 表示测试文件和被测试代码在同一个包里。
//
// 这样测试可以直接调用 parseByteSize、newRotatingFile 这些未导出的函数。
package main

// import 是测试需要用到的标准库。
//
// os：检查文件是否存在。
// filepath：拼接临时目录路径。
// testing：Go 标准测试包。
import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseByteSize 测试 "10MB" 这类配置值能不能正确转成字节数。
//
// Go 的测试函数命名必须以 Test 开头，并接收 *testing.T。
// t 用来报告测试失败。
func TestParseByteSize(t *testing.T) {
	// map[string]int64 是一个“字符串 -> int64”的表。
	// 这里用它写多组输入输出，也叫 table-driven test。
	tests := map[string]int64{
		"10MB": 10 * 1024 * 1024,
		"2m":   2 * 1024 * 1024,
		"512K": 512 * 1024,
		"42":   42,
	}
	// range 遍历 map。input 是 key，want 是期望值。
	for input, want := range tests {
		got, err := parseByteSize(input)
		if err != nil {
			// t.Fatalf 会立刻让当前测试失败并停止。
			t.Fatalf("parseByteSize(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestRotatingFileRotatesAndKeepsBackups 测试日志文件超过大小后会轮转，并保留指定数量备份。
func TestRotatingFileRotatesAndKeepsBackups(t *testing.T) {
	// t.TempDir() 创建测试专用临时目录，测试结束后 Go 会自动清理。
	path := filepath.Join(t.TempDir(), "scheduler.log")
	// maxBytes=10，maxBackups=2，方便用很短字符串触发轮转。
	file, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatalf("newRotatingFile returned error: %v", err)
	}
	// 第一段正好 10 字节，不会立刻轮转。
	if _, err := file.Write([]byte("1234567890")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	// 第二段会超过 10 字节，所以写入前会轮转一次。
	if _, err := file.Write([]byte("abc")); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if _, err := file.Write([]byte("defghijklm")); err != nil {
		t.Fatalf("write third chunk: %v", err)
	}
	if _, err := file.Write([]byte("xyz")); err != nil {
		t.Fatalf("write fourth chunk: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close returned error: %v", err)
	}

	// os.Stat 能读取文件信息；如果文件不存在会返回错误。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("first backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("second backup missing: %v", err)
	}
	// maxBackups=2，所以 .3 不应该存在。
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third backup stat error = %v", err)
	}
}
