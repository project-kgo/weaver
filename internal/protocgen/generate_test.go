package protocgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"google.golang.org/protobuf/types/pluginpb"
)

func TestGenerateGolden(t *testing.T) {
	content, err := generateTestFile(false)
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "simple.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != string(want) {
		t.Fatalf("generated output differs from %s\n--- got ---\n%s", goldenPath, content)
	}
	second, err := generateTestFile(false)
	if err != nil {
		t.Fatal(err)
	}
	if content != second {
		t.Fatal("generator output is not deterministic")
	}
}

func TestRejectStreaming(t *testing.T) {
	_, err := generateTestFile(true)
	if err == nil || !strings.Contains(err.Error(), "仅支持 unary RPC") {
		t.Fatalf("expected streaming error, got %v", err)
	}
}

func generateTestFile(streaming bool) (string, error) {
	wrappersDescriptor := protodesc.ToFileDescriptorProto(wrapperspb.File_google_protobuf_wrappers_proto)
	request := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"test/v1/test.proto"},
		Parameter:      proto.String("paths=source_relative"),
		ProtoFile: []*descriptorpb.FileDescriptorProto{
			wrappersDescriptor,
			{
				Name:       proto.String("test/v1/test.proto"),
				Package:    proto.String("test.v1"),
				Dependency: []string{"google/protobuf/wrappers.proto"},
				Options: &descriptorpb.FileOptions{
					GoPackage: proto.String("example.com/project/gen/test/v1;testv1"),
				},
				Service: []*descriptorpb.ServiceDescriptorProto{
					{
						Name: proto.String("EchoService"),
						Method: []*descriptorpb.MethodDescriptorProto{
							{
								Name:            proto.String("Echo"),
								InputType:       proto.String(".google.protobuf.StringValue"),
								OutputType:      proto.String(".google.protobuf.StringValue"),
								ServerStreaming: proto.Bool(streaming),
							},
						},
					},
				},
			},
		},
	}
	plugin, err := (protogen.Options{}).New(request)
	if err != nil {
		return "", err
	}
	if err := Generate(plugin); err != nil {
		return "", err
	}
	response := plugin.Response()
	if len(response.File) != 1 {
		return "", fmt.Errorf("unexpected generated file count: %d", len(response.File))
	}
	return response.File[0].GetContent(), nil
}
