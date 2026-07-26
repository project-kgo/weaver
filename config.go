package weaver

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config 描述部署单元及 protobuf service 到单元的映射。
type Config struct {
	Units      map[string]string `yaml:"units"`
	Placements map[string]string `yaml:"placements"`
}

// ParseConfig 严格解析 YAML 配置，未知字段会直接报错。
func ParseConfig(data []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("weaver: 解析配置失败: %w", err)
	}
	if config.Units == nil {
		config.Units = make(map[string]string)
	}
	if config.Placements == nil {
		config.Placements = make(map[string]string)
	}
	return config, nil
}
