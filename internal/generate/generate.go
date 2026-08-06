package generate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

const (
	weaverPackagePath = "github.com/project-kgo/weaver"
	generatedFilename = "zz_weaver_gen.go"
)

// Output 是一次生成操作产生的文件变更。
type Output struct {
	Filename string
	Content  []byte
	Remove   bool
}

type componentModel struct {
	implementation string
	componentType  types.Type
	descriptor     descriptorModel
	config         *fieldModel
	refs           []fieldModel
	resources      []fieldModel
}

type descriptorModel struct {
	packageRef *types.Package
	name       string
}

type fieldModel struct {
	name       string
	valueType  types.Type
	descriptor descriptorModel
}

type packageModel struct {
	name       string
	path       string
	directory  string
	components []componentModel
}

// Generate 扫描 Go package 并在内存中生成注册与注入代码。
func Generate(ctx context.Context, directory string, patterns ...string) ([]Output, error) {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	config := &packages.Config{
		Context: ctx,
		Dir:     directory,
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedModule,
		ParseFile: parseSourceFile,
	}
	loaded, err := packages.Load(config, patterns...)
	if err != nil {
		return nil, fmt.Errorf("weaver generate: 加载 package 失败: %w", err)
	}
	if err := packageErrors(loaded); err != nil {
		return nil, err
	}

	models := make([]packageModel, 0, len(loaded))
	seenComponents := make(map[string]string)
	for _, loadedPackage := range loaded {
		model, err := scanPackage(loadedPackage)
		if err != nil {
			return nil, err
		}
		for _, component := range model.components {
			identity := types.TypeString(component.componentType, func(pkg *types.Package) string { return pkg.Path() })
			location := model.path + "." + component.implementation
			if previous, exists := seenComponents[identity]; exists {
				return nil, fmt.Errorf("weaver generate: 组件接口 %s 同时由 %s 和 %s 实现", identity, previous, location)
			}
			seenComponents[identity] = location
		}
		models = append(models, model)
	}
	sort.Slice(models, func(left, right int) bool { return models[left].path < models[right].path })

	outputs := make([]Output, 0, len(models))
	for _, model := range models {
		filename := filepath.Join(model.directory, generatedFilename)
		if len(model.components) == 0 {
			if _, err := os.Stat(filename); err == nil {
				outputs = append(outputs, Output{Filename: filename, Remove: true})
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		content, err := renderPackage(model)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, Output{Filename: filename, Content: content})
	}
	return outputs, nil
}

// Write 原子写入 Generate 返回的文件变更。
func Write(outputs []Output) error {
	for _, output := range outputs {
		if output.Remove {
			if err := os.Remove(output.Filename); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("weaver generate: 删除 %s 失败: %w", output.Filename, err)
			}
			continue
		}
		if err := writeAtomic(output.Filename, output.Content); err != nil {
			return err
		}
	}
	return nil
}

func parseSourceFile(fileSet *token.FileSet, filename string, source []byte) (*ast.File, error) {
	if filepath.Base(filename) != generatedFilename {
		return parser.ParseFile(fileSet, filename, source, parser.ParseComments)
	}
	// 旧生成文件可能已与源码不兼容，只保留 package 子句参与本轮类型检查。
	parsed, err := parser.ParseFile(fileSet, filename, source, parser.PackageClauseOnly)
	if err != nil {
		return nil, err
	}
	return parser.ParseFile(fileSet, filename, []byte("package "+parsed.Name.Name+"\n"), 0)
}

func packageErrors(loaded []*packages.Package) error {
	var messages []string
	for _, loadedPackage := range loaded {
		for _, packageError := range loadedPackage.Errors {
			messages = append(messages, packageError.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	sort.Strings(messages)
	return fmt.Errorf("weaver generate: package 存在错误:\n%s", strings.Join(messages, "\n"))
}

func scanPackage(loaded *packages.Package) (packageModel, error) {
	model := packageModel{name: loaded.Name, path: loaded.PkgPath}
	for _, filename := range loaded.GoFiles {
		if filepath.Base(filename) != generatedFilename {
			model.directory = filepath.Dir(filename)
			break
		}
	}
	if model.directory == "" && len(loaded.GoFiles) != 0 {
		model.directory = filepath.Dir(loaded.GoFiles[0])
	}
	if loaded.Types == nil || model.directory == "" {
		return model, nil
	}

	scope := loaded.Types.Scope()
	for _, name := range scope.Names() {
		object, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || object.IsAlias() {
			continue
		}
		named, ok := object.Type().(*types.Named)
		if !ok {
			continue
		}
		structure, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		component, found, err := scanComponent(object.Name(), structure)
		if err != nil {
			return packageModel{}, fmt.Errorf("weaver generate: %s.%s: %w", loaded.PkgPath, object.Name(), err)
		}
		if found {
			model.components = append(model.components, component)
		}
	}
	sort.Slice(model.components, func(left, right int) bool {
		return model.components[left].implementation < model.components[right].implementation
	})
	return model, nil
}

func scanComponent(implementation string, structure *types.Struct) (componentModel, bool, error) {
	model := componentModel{implementation: implementation}
	foundImplementation := false
	for index := 0; index < structure.NumFields(); index++ {
		field := structure.Field(index)
		kind, argument, matched := weaverGeneric(field.Type())
		if !matched {
			continue
		}
		switch kind {
		case "Implements":
			if !field.Embedded() {
				return componentModel{}, false, fmt.Errorf("Implements[T] 必须匿名嵌入")
			}
			if foundImplementation {
				return componentModel{}, false, fmt.Errorf("只能声明一个 Implements[T]")
			}
			descriptor, err := componentDescriptor(argument)
			if err != nil {
				return componentModel{}, false, err
			}
			model.componentType = argument
			model.descriptor = descriptor
			foundImplementation = true
		case "Ref":
			descriptor, err := componentDescriptor(argument)
			if err != nil {
				return componentModel{}, false, fmt.Errorf("字段 %s: %w", field.Name(), err)
			}
			model.refs = append(model.refs, fieldModel{name: field.Name(), valueType: argument, descriptor: descriptor})
		case "Resource":
			model.resources = append(model.resources, fieldModel{name: field.Name(), valueType: argument})
		case "WithConfig":
			if !field.Embedded() {
				return componentModel{}, false, fmt.Errorf("WithConfig[T] 必须匿名嵌入")
			}
			if model.config != nil {
				return componentModel{}, false, fmt.Errorf("只能声明一个 WithConfig[T]")
			}
			if _, ok := argument.Underlying().(*types.Struct); !ok {
				return componentModel{}, false, fmt.Errorf("WithConfig[T] 的 T 必须是结构体，得到 %s", types.TypeString(argument, nil))
			}
			model.config = &fieldModel{name: field.Name(), valueType: argument}
		}
	}
	return model, foundImplementation, nil
}

func weaverGeneric(value types.Type) (string, types.Type, bool) {
	named, ok := types.Unalias(value).(*types.Named)
	if !ok || named.TypeArgs().Len() != 1 {
		return "", nil, false
	}
	origin := named.Origin()
	object := origin.Obj()
	if object.Pkg() == nil || object.Pkg().Path() != weaverPackagePath {
		return "", nil, false
	}
	switch object.Name() {
	case "Implements", "Ref", "Resource", "WithConfig":
		return object.Name(), types.Unalias(named.TypeArgs().At(0)), true
	default:
		return "", nil, false
	}
}

func componentDescriptor(componentType types.Type) (descriptorModel, error) {
	named, ok := types.Unalias(componentType).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return descriptorModel{}, fmt.Errorf("组件类型必须是命名接口，得到 %s", types.TypeString(componentType, nil))
	}
	if _, ok := named.Underlying().(*types.Interface); !ok {
		return descriptorModel{}, fmt.Errorf("组件类型 %s 不是接口", named.Obj().Name())
	}
	name := named.Obj().Name()
	if !strings.HasSuffix(name, "Component") {
		return descriptorModel{}, fmt.Errorf("组件接口 %s 必须以 Component 结尾", name)
	}
	return descriptorModel{packageRef: named.Obj().Pkg(), name: strings.TrimSuffix(name, "Component")}, nil
}

type importManager struct {
	currentPath string
	byPath      map[string]string
	usedAliases map[string]string
}

func newImportManager(currentPath string) *importManager {
	manager := &importManager{
		currentPath: currentPath,
		byPath:      make(map[string]string),
		usedAliases: make(map[string]string),
	}
	manager.byPath[weaverPackagePath] = "weaver"
	manager.usedAliases["weaver"] = weaverPackagePath
	return manager
}

func (m *importManager) qualifier(pkg *types.Package) string {
	if pkg == nil || pkg.Path() == m.currentPath {
		return ""
	}
	if alias, exists := m.byPath[pkg.Path()]; exists {
		return alias
	}
	base := sanitizeIdentifier(pkg.Name())
	if base == "" {
		base = "pkg"
	}
	alias := base
	for suffix := 2; ; suffix++ {
		if existing, exists := m.usedAliases[alias]; !exists || existing == pkg.Path() {
			break
		}
		alias = base + strconv.Itoa(suffix)
	}
	m.byPath[pkg.Path()] = alias
	m.usedAliases[alias] = pkg.Path()
	return alias
}

func (m *importManager) reserve(importPath, preferred string) string {
	if alias, exists := m.byPath[importPath]; exists {
		return alias
	}
	base := sanitizeIdentifier(preferred)
	if base == "" {
		base = "pkg"
	}
	alias := base
	for suffix := 2; ; suffix++ {
		if existing, exists := m.usedAliases[alias]; !exists || existing == importPath {
			break
		}
		alias = base + strconv.Itoa(suffix)
	}
	m.byPath[importPath] = alias
	m.usedAliases[alias] = importPath
	return alias
}

func (m *importManager) descriptor(value descriptorModel) string {
	alias := m.qualifier(value.packageRef)
	if alias == "" {
		return value.name
	}
	return alias + "." + value.name
}

func renderPackage(model packageModel) ([]byte, error) {
	imports := newImportManager(model.path)
	for _, component := range model.components {
		if component.config != nil {
			imports.reserve("reflect", "reflect")
			break
		}
	}
	var body bytes.Buffer
	body.WriteString("var _ [weaver.CodegenVersion]struct{} = [3]struct{}{}\n\n")
	body.WriteString("func init() {\n")
	for _, component := range model.components {
		renderRegistration(&body, imports, component)
	}
	body.WriteString("}\n\n")
	for _, component := range model.components {
		componentType := types.TypeString(component.componentType, imports.qualifier)
		fmt.Fprintf(&body, "var _ %s = (*%s)(nil)\n", componentType, component.implementation)
	}

	paths := make([]string, 0, len(imports.byPath))
	for importPath := range imports.byPath {
		if importPath != model.path {
			paths = append(paths, importPath)
		}
	}
	sort.Strings(paths)

	var source bytes.Buffer
	source.WriteString("// Code generated by weaver generate. DO NOT EDIT.\n\n")
	fmt.Fprintf(&source, "package %s\n\n", model.name)
	source.WriteString("import (\n")
	for _, importPath := range paths {
		fmt.Fprintf(&source, "\t%s %q\n", imports.byPath[importPath], importPath)
	}
	source.WriteString(")\n\n")
	source.Write(body.Bytes())

	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, fmt.Errorf("weaver generate: 格式化 %s 失败: %w\n%s", model.path, err, source.String())
	}
	return formatted, nil
}

func renderRegistration(output *bytes.Buffer, imports *importManager, component componentModel) {
	descriptor := imports.descriptor(component.descriptor)
	fmt.Fprintln(output, "\tweaver.MustRegister(weaver.Registration{")
	fmt.Fprintf(output, "\t\tService: %s.Descriptor(),\n", descriptor)
	if component.config != nil {
		typeName := types.TypeString(component.config.valueType, imports.qualifier)
		fmt.Fprintf(output, "\t\tConfigType: %s.TypeFor[%s](),\n", imports.byPath["reflect"], typeName)
	}
	fmt.Fprintf(output, "\t\tNew: func() any { return new(%s) },\n", component.implementation)
	fmt.Fprintln(output, "\t\tInject: func(target any, injector weaver.Injector) error {")
	if len(component.refs) != 0 || len(component.resources) != 0 || component.config != nil {
		fmt.Fprintf(output, "\t\t\tcomponent := target.(*%s)\n", component.implementation)
	}
	for index, field := range component.refs {
		typeName := types.TypeString(field.valueType, imports.qualifier)
		dependency := imports.descriptor(field.descriptor)
		fmt.Fprintf(output, "\t\t\tref%d, err := weaver.ResolveComponent[%s](injector, %s.Name)\n", index, typeName, dependency)
		fmt.Fprintln(output, "\t\t\tif err != nil {")
		fmt.Fprintln(output, "\t\t\t\treturn err")
		fmt.Fprintln(output, "\t\t\t}")
		fmt.Fprintf(output, "\t\t\tweaver.SetRef(&component.%s, ref%d)\n", field.name, index)
	}
	for index, field := range component.resources {
		typeName := types.TypeString(field.valueType, imports.qualifier)
		fmt.Fprintf(output, "\t\t\tresource%d, err := weaver.ResolveResource[%s](injector)\n", index, typeName)
		fmt.Fprintln(output, "\t\t\tif err != nil {")
		fmt.Fprintln(output, "\t\t\t\treturn err")
		fmt.Fprintln(output, "\t\t\t}")
		fmt.Fprintf(output, "\t\t\tweaver.SetResource(&component.%s, resource%d)\n", field.name, index)
	}
	if component.config != nil {
		typeName := types.TypeString(component.config.valueType, imports.qualifier)
		fmt.Fprintf(output, "\t\t\tconfig, err := weaver.ResolveConfig[%s](injector)\n", typeName)
		fmt.Fprintln(output, "\t\t\tif err != nil {")
		fmt.Fprintln(output, "\t\t\t\treturn err")
		fmt.Fprintln(output, "\t\t\t}")
		fmt.Fprintf(output, "\t\t\tweaver.SetConfig(&component.%s, config)\n", component.config.name)
	}
	fmt.Fprintln(output, "\t\t\treturn nil")
	fmt.Fprintln(output, "\t\t},")

	dependencies := make(map[string]descriptorModel)
	for _, field := range component.refs {
		key := field.descriptor.packageRef.Path() + "." + field.descriptor.name
		dependencies[key] = field.descriptor
	}
	keys := make([]string, 0, len(dependencies))
	for key := range dependencies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 0 {
		fmt.Fprintln(output, "\t\tDependencies: []string{")
		for _, key := range keys {
			fmt.Fprintf(output, "\t\t\t%s.Name,\n", imports.descriptor(dependencies[key]))
		}
		fmt.Fprintln(output, "\t\t},")
	}
	fmt.Fprintln(output, "\t})")
}

func sanitizeIdentifier(value string) string {
	var result strings.Builder
	for index, character := range value {
		if character == '_' || unicode.IsLetter(character) || (index > 0 && unicode.IsDigit(character)) {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func writeAtomic(filename string, content []byte) error {
	directory := filepath.Dir(filename)
	temporary, err := os.CreateTemp(directory, ".weaver-generate-*")
	if err != nil {
		return fmt.Errorf("weaver generate: 创建临时文件失败: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("weaver generate: 写入 %s 失败: %w", filename, err)
	}
	return nil
}
