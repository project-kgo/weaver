package weaver

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Config 描述部署单元及 protobuf service 到单元的映射。
type Config struct {
	Units      map[string]string `yaml:"units"`
	Placements map[string]string `yaml:"placements"`

	// componentConfigs 保存按 protobuf service 全名索引的原始 YAML 配置段。
	// 具体类型只有生成代码注册完成后才能确定，因此延迟到 Runtime 启动时解码。
	componentConfigs map[string][]byte
}

type configFile struct {
	Units            map[string]string    `yaml:"units"`
	Placements       map[string]string    `yaml:"placements"`
	ComponentConfigs map[string]yaml.Node `yaml:",inline"`
}

// ParseConfig 严格解析 YAML 配置。除 units 和 placements 外，顶层键必须是
// placements 中声明的 protobuf service 全名。
func ParseConfig(data []byte) (Config, error) {
	var parsed configFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil {
		return Config{}, fmt.Errorf("weaver: 解析配置失败: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("weaver: 解析配置失败: 只允许一个 YAML 文档")
		}
		return Config{}, fmt.Errorf("weaver: 解析配置失败: %w", err)
	}

	config := Config{
		Units:            parsed.Units,
		Placements:       parsed.Placements,
		componentConfigs: make(map[string][]byte, len(parsed.ComponentConfigs)),
	}
	if config.Units == nil {
		config.Units = make(map[string]string)
	}
	if config.Placements == nil {
		config.Placements = make(map[string]string)
	}
	for name, node := range parsed.ComponentConfigs {
		if _, exists := config.Placements[name]; !exists {
			return Config{}, fmt.Errorf("weaver: 组件配置段 %q 未出现在 placements 中", name)
		}
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return Config{}, fmt.Errorf("weaver: 编码组件配置段 %q 失败: %w", name, err)
		}
		config.componentConfigs[name] = encoded
	}
	return config, nil
}
