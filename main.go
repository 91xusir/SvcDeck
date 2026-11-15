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
	"syscall"
	"time"
)

// 单二进制交付：前端 index.html 直接 embed，不依赖外部目录。
//
//go:embed web/index.html
var webFS embed.FS

const singletonName = `Local\SvcDeck_Panel_Singleton_v1`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Printf("[panel] 退出: %v", err)
		os.Exit(1)
	}
}

func run() error {
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
	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler())
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
