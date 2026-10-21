package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ts3panel/internal/api"
	"ts3panel/internal/store"
)

var (
	version   = "1.0.1"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ts3panel: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runServe(nil)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "reset-password":
		return runResetPassword(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("ts3panel %s (commit %s, built %s)\n", version, commit, buildDate)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			return runServe(args)
		}
		return fmt.Errorf("未知子命令 %q（运行 ts3panel help 查看用法）", args[0])
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", store.DefaultConfigPath, "配置文件路径")
	statePath := fs.String("state", store.DefaultStatePath, "state.json 路径（需与 reset-password 保持一致）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve 不接受位置参数: %s", strings.Join(fs.Args(), " "))
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	configStore, created, err := store.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if created {
		slog.Info("config.toml generated", "path", configStore.Path())
	}
	stateStore, err := store.OpenState(resolveStatePath(*statePath))
	if err != nil {
		return err
	}
	cfg := configStore.Snapshot()

	handler, err := api.New(api.Options{
		Config:  configStore,
		State:   stateStore,
		Version: version,
	})
	if err != nil {
		return fmt.Errorf("initialize api: %w", err)
	}
	defer handler.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	slog.Info("ts3panel starting",
		"version", version,
		"listen", cfg.Listen,
		"config", configStore.Path(),
		"state", stateStore.Path(),
	)

	select {
	case <-ctx.Done():
		slog.Info("ts3panel shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown http server: %w", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	}
}

func runResetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", store.DefaultConfigPath, "配置文件路径")
	statePath := fs.String("state", store.DefaultStatePath, "state.json 路径（需与 serve 保持一致）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("reset-password 不接受位置参数: %s", strings.Join(fs.Args(), " "))
	}

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		return errors.New("reset-password 必须以 root 身份运行（sudo ts3panel reset-password）")
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "警告：Windows 不是受支持的宿主机；已跳过 root 检查。")
	}

	if _, err := os.Stat(*configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("配置文件不存在: %s（请先完成安装并启动面板）", *configPath)
		}
		return fmt.Errorf("检查配置文件: %w", err)
	}
	configStore, _, err := store.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	stateStore, err := store.OpenExistingState(resolveStatePath(*statePath))
	if err != nil {
		return fmt.Errorf("打开 state.json: %w（面板尚未初始化或 --state 路径不正确）", err)
	}

	token, tokenRecord, err := store.NewSetupToken(5 * time.Minute)
	if err != nil {
		return err
	}
	if err := stateStore.Update(func(st *store.State) error {
		st.SetupToken = &tokenRecord
		return nil
	}); err != nil {
		return fmt.Errorf("写入 setup token: %w", err)
	}

	cfg := configStore.Snapshot()
	fmt.Println("一次性账号重置链接（5 分钟内有效，仅可使用一次）：")
	fmt.Println(setupURL(cfg.Listen, token))
	fmt.Println("如面板不在默认监听地址，请按 config.toml 的 listen 调整；建议通过 SSH 隧道访问 127.0.0.1。")
	return nil
}

func resolveStatePath(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return store.DefaultStatePath
}

func setupURL(listen, token string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		host, port = "127.0.0.1", "8899"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8899"
	}
	return "http://" + net.JoinHostPort(host, port) + "/setup?token=" + url.QueryEscape(token)
}

func printUsage() {
	fmt.Print(`ts3panel - TeamSpeak 3 多实例管理面板

用法:
  ts3panel [serve] [--config PATH] [--state PATH]
  ts3panel reset-password [--config PATH] [--state PATH]
  ts3panel version

说明:
  serve           启动面板（默认子命令），默认监听 127.0.0.1:8899
  reset-password  生成一次性账号重置链接（需要 root；5 分钟有效）
  version         输出版本与构建信息

默认路径:
  config: /etc/ts3panel/config.toml
  state:  /var/lib/ts3panel/state.json

重置链接示例:
  http://127.0.0.1:8899/setup?token=...
`)
}
