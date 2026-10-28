package generate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateGolden(t *testing.T) {
	root := repositoryRoot(t)
	outputs, err := Generate(context.Background(), root, "./internal/generate/testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 || outputs[0].Remove {
		t.Fatalf("unexpected outputs: %#v", outputs)
	}
	goldenPath := filepath.Join("testdata", "app.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, outputs[0].Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(outputs[0].Content) != string(want) {
		t.Fatalf("generated output differs from %s\n--- got ---\n%s", goldenPath, outputs[0].Content)
	}
	second, err := Generate(context.Background(), root, "./internal/generate/testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || string(second[0].Content) != string(outputs[0].Content) {
		t.Fatal("generator output is not deterministic")
	}
}

func TestGenerateRejectsInvalidComponentName(t *testing.T) {
	_, err := Generate(context.Background(), repositoryRoot(t), "./internal/generate/testdata/invalid")
	if err == nil || !strings.Contains(err.Error(), "必须以 Component 结尾") {
		t.Fatalf("expected component naming error, got %v", err)
	}
}

func TestGenerateRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{pattern: "./internal/generate/testdata/invalid_config_named", want: "WithConfig[T] 必须匿名嵌入"},
		{pattern: "./internal/generate/testdata/invalid_config_multiple", want: "只能声明一个 WithConfig[T]"},
		{pattern: "./internal/generate/testdata/invalid_config_type", want: "T 必须是结构体"},
	}
	for _, test := range tests {
		t.Run(filepath.Base(test.pattern), func(t *testing.T) {
			_, err := Generate(context.Background(), repositoryRoot(t), test.pattern)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestGenerateRejectsInvalidHTTPComponent(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{pattern: "./internal/generate/testdata/invalid_http_signature", want: "签名必须是 func(http.ResponseWriter, *http.Request)"},
		{pattern: "./internal/generate/testdata/invalid_http_name", want: "HTTP 组件名称 \"test/bad\" 无效"},
		{pattern: "./internal/generate/testdata/invalid_http_implements", want: "HTTP 组件不能同时声明 Implements[T]"},
		{pattern: "./internal/generate/testdata/invalid_http_no_routes", want: "没有任何 //weaver:route 路由"},
		{pattern: "./internal/generate/testdata/invalid_http_orphan_route", want: "类型缺少 //weaver:http"},
		{pattern: "./internal/generate/testdata/invalid_http_conflict", want: "conflicts with pattern"},
		{pattern: "./internal/generate/testdata/invalid_http_unknown_directive", want: "无法识别或位置无效的指令"},
		{pattern: "./internal/generate/testdata/invalid_http_function", want: "//weaver:route 只能用于方法"},
	}
	for _, test := range tests {
		t.Run(filepath.Base(test.pattern), func(t *testing.T) {
			_, err := Generate(context.Background(), repositoryRoot(t), test.pattern)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestWriteAtomicAndRemove(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, generatedFilename)
	if err := Write([]Output{{Filename: filename, Content: []byte("package fixture\n")}}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filename)
	if err != nil || string(content) != "package fixture\n" {
		t.Fatalf("unexpected file: %q, %v", content, err)
	}
	if err := Write([]Output{{Filename: filename, Remove: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Fatalf("expected file removal, got %v", err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(directory, "..", ".."))
}
