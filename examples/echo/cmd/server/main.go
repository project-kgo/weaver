package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/project-kgo/weaver"
	_ "github.com/project-kgo/weaver/examples/echo/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	unit := flag.String("unit", os.Getenv("APP_UNIT"), "当前部署单元")
	configPath := flag.String("config", "config/monolith.yaml", "Weaver YAML 配置")
	listenAddress := flag.String("listen", ":8080", "HTTP 监听地址")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	config, err := weaver.ParseConfig(data)
	if err != nil {
		return err
	}

	return weaver.Serve(context.Background(), *unit, config, *listenAddress)
}
