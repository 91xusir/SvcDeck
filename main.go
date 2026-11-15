package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"svcdeck/mcpserver"
)

// 单二进制交付：前端 index.html 直接 embed，不依赖外部目录。
//
//go:embed web/index.html
var webFS embed.FS

const singletonName = `Local\SvcDeck_Panel_Singleton_v1`

func main() {
	// MCP 模式必须在 attachParentConsole / 单例 / flag.Parse 之前分流：
	// stdio MCP 独占 stdin/stdout，不能把标准流重绑到父控制台，也不能去抢面板单例。
	if isMCPMode(os.Args[1:]) {
		if err := runMCP(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "svcdeck-mcp:", err)
			os.Exit(1)
		}
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Printf("[panel] 退出: %v", err)
		os.Exit(1)
	}
}

func isMCPMode(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "mcp", "-mcp", "--mcp":
		return true
	}
	return false
}

// runMCP 用同一个 SvcDeck.exe 以 stdio MCP Server 运行，不再单独编译 exe。
// 它只通过面板 HTTP API 操作；面板可以继续以普通模式运行。
func runMCP(args []string) error {
	log.SetFlags(0)
	log.SetPrefix("[svcdeck-mcp] ")

	cfg, err := mcpserver.LoadConfig()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("svcdeck-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	urlOverride := fs.String("url", "", "面板地址（覆盖 SVCDECK_URL，例如 http://127.0.0.1:8088）")
	tokenOverride := fs.String("token", "", "面板访问令牌（覆盖 SVCDECK_TOKEN；建议用环境变量，避免出现在命令行）")

	clean := args
	if len(args) > 0 {
		switch args[0] {
		case "mcp", "-mcp", "--mcp":
			clean = args[1:]
		}
	}
	if err := fs.Parse(clean); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *urlOverride != "" {
		cfg.BaseURL = *urlOverride
	}
	if *tokenOverride != "" {
		cfg.Token = *tokenOverride
	}

	return mcpserver.Run(cfg, os.Stdin, os.Stdout)
}

func run() error {
	// 0) 控制台：windowsgui 构建的进程默认没有终端。
	//    从 cmd/PowerShell 启动时接上父控制台，日志直接打在终端；
	//    双击启动时接不上，静默运行（日志改写到 logs/panel.log，见下）。
	//    必须在 flag.Parse 之前，否则 -h / 参数报错的信息也没有地方可写。
	hasConsole := attachParentConsole()
	if hasConsole {
		// log 包在 init 时已抓取旧的 os.Stderr，重定向后必须显式重设。
		log.SetOutput(os.Stderr)
	}

	var (
		dataDir string
		dbPath  string
		addr    string
		token   string
	)
	flag.StringVar(&dataDir, "data", "", "数据目录（默认与 Panel 可执行文件同目录）")
	flag.StringVar(&dbPath, "db", "", "SQLite 文件路径（默认 <data>/panel.db）")
	flag.StringVar(&addr, "addr", "", "监听地址（覆盖数据库中的 listen_addr，例如 127.0.0.1:8088）")
	flag.StringVar(&token, "token", "", "访问令牌（覆盖数据库中的 token；留空则使用数据库配置）")
	flag.Parse()

	// 1) 单例：命名 Mutex，进程退出/崩溃自动释放
	release, err := acquireSingleInstance(singletonName)
	if err != nil {
		return err
	}
	defer release()

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取自身路径失败: %w", err)
	}
	baseDir := filepath.Dir(exePath)
	if dataDir == "" {
		dataDir = baseDir
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "panel.db")
	}
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败: %w", logDir, err)
	}
	if !hasConsole {
		// 双击启动没有终端，stderr 是无效句柄，日志会全部丢失：落盘留痕。
		if f, err := os.OpenFile(filepath.Join(logDir, "panel.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			log.SetOutput(f)
			defer f.Close()
		}
	}

	// 2) SQLite：全部数据（配置、运行态、设置、探测历史）
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	rows, err := store.LoadSettings()
	if err != nil {
		return fmt.Errorf("读取设置失败: %w", err)
	}
	settings := settingsFromRows(rows)
	if len(rows) == 0 {
		if err := store.SaveSettings(configToRows(settings)); err != nil {
			return fmt.Errorf("写入默认设置失败: %w", err)
		}
	}
	if addr != "" {
		settings.ListenAddr = addr
	}
	if token != "" {
		settings.Token = token
	}
	if err := settings.validate(); err != nil {
		return fmt.Errorf("设置非法: %w", err)
	}
	cfgStore := NewConfigStore(settings)

	// 3) 监督器：加载配置、按拓扑序 adopt/spawn、启动探针循环
	sup, err := NewSupervisor(store, cfgStore, logDir)
	if err != nil {
		return fmt.Errorf("初始化服务监督器失败: %w", err)
	}
	sup.Start()

	api := NewAPI(store, sup, cfgStore)

	mcpHTTP, err := mcpserver.NewHTTPHandler(mcpserver.Config{
		BaseURL: panelSelfURL(settings.ListenAddr),
		Token:   settings.Token,
		Timeout: 15 * time.Second,
		// 面板运行中改 token 后，MCP 内部调用也要立刻用新 token；
		// 外部客户端仍需在请求头里带同一个 token（见 README §6.3）。
		TokenSource: func() string { return cfgStore.Get().Token },
	})
	if err != nil {
		return fmt.Errorf("初始化 MCP HTTP 端点失败: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler())
	mux.Handle("/mcp", api.authMW(mcpHTTP))
	mux.HandleFunc("/", serveIndex)

	ln, err := net.Listen("tcp", settings.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", settings.ListenAddr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(os.Stderr, "[http] ", log.LstdFlags),
	}

	log.Printf("[panel] SvcDeck 已启动：http://%s  db=%s  logs=%s", settings.ListenAddr, dbPath, logDir)
	log.Printf("[panel] MCP HTTP 端点：http://%s/mcp（同一个面板进程，无额外 exe）", settings.ListenAddr)
	log.Printf("[panel] 面板只是拉起者/探活者；退出时不会终止任何业务进程")

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 4) 优雅退出：只关 HTTP 与探针循环，绝不 taskkill 子进程。
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-sigCtx.Done():
		log.Printf("[panel] 收到退出信号，开始优雅关闭（业务进程保持运行）")
	case err := <-errCh:
		log.Printf("[panel] HTTP 服务异常: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[panel] HTTP 关闭超时: %v", err)
	}
	sup.Close(shutdownCtx)
	// store.Close 由 defer 执行；释放 Mutex 由 defer 执行。
	log.Printf("[panel] 已退出；子进程未被终止，下次启动会通过 adopt 接管")
	return nil
}

// panelSelfURL 把监听地址转成 MCP 内部回调自己的 URL。
// 0.0.0.0 / :: 这类通配地址不能直接拨号，换成 loopback。
func panelSelfURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://127.0.0.1:8088"
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "[::1]"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + port
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "embedded web/index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 单页内联脚本/样式，因此 CSP 必须允许 inline；其余默认收紧。
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
