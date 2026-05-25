package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wywang792/claude-tap-go/internal/ca"
	"github.com/wywang792/claude-tap-go/internal/client"
	"github.com/wywang792/claude-tap-go/internal/history"
	"github.com/wywang792/claude-tap-go/internal/live"
	"github.com/wywang792/claude-tap-go/internal/proxy"
	"github.com/wywang792/claude-tap-go/internal/trace"
)

const version = "0.1.0"

func main() {
	os.Exit(mainEntry())
}

func mainEntry() int {
	// Parse args
	var (
		tapPort           int
		tapHost           string
		tapClient         string
		tapTarget         string
		tapProxyMode      string
		tapNoLaunch       bool
		tapNoOpen         bool
		tapLive           bool
		tapNoLive         bool
		tapLivePort       int
		tapOutputDir      string
		tapMaxTraces      int
		tapNoUpdateCheck  bool
		tapConsoleLog     bool
		extraAllowedPaths string
	)

	flags := flag.NewFlagSet("claude-tap", flag.ExitOnError)
	flags.IntVar(&tapPort, "tap-port", 0, "Proxy port (0=auto)")
	flags.StringVar(&tapHost, "tap-host", "127.0.0.1", "Proxy bind address")
	flags.StringVar(&tapClient, "tap-client", "claude", "Client to launch (claude/codex/kimi/gemini/opencode/hermes/cursor/pi/qoder/agy)")
	flags.StringVar(&tapTarget, "tap-target", "", "Upstream API URL (auto-detected if empty)")
	flags.StringVar(&tapProxyMode, "tap-proxy-mode", "", "Proxy mode: reverse or forward (default: per-client)")
	flags.BoolVar(&tapNoLaunch, "tap-no-launch", false, "Proxy only, don't launch client")
	flags.BoolVar(&tapNoOpen, "tap-no-open", false, "Don't open browser")
	flags.BoolVar(&tapLive, "tap-live", true, "Enable live viewer")
	flags.BoolVar(&tapNoLive, "tap-no-live", false, "Disable live viewer")
	flags.IntVar(&tapLivePort, "tap-live-port", 0, "Live viewer port (0=auto)")
	flags.StringVar(&tapOutputDir, "tap-output-dir", ".traces", "Trace output directory")
	flags.IntVar(&tapMaxTraces, "tap-max-traces", 50, "Max traces to retain (0=unlimited)")
	flags.BoolVar(&tapNoUpdateCheck, "tap-no-update-check", false, "Skip update check")
	flags.BoolVar(&tapConsoleLog, "tap-console-log", false, "Mirror proxy request logs to stderr")
	flags.StringVar(&extraAllowedPaths, "tap-allow-path", "", "Extra allowed path prefixes (comma-separated)")

	// Export subcommand
	exportCmd := flag.NewFlagSet("export", flag.ExitOnError)
	exportOutput := exportCmd.String("o", "", "Output file (.md/.json/.html)")
	exportFormat := exportCmd.String("format", "markdown", "Output format")

	// Trust CA subcommand
	trustCACmd := flag.NewFlagSet("trust-ca", flag.ExitOnError)

	// Dashboard subcommand
	dashboardCmd := flag.NewFlagSet("dashboard", flag.ExitOnError)
	dashboardPort := dashboardCmd.Int("port", 8989, "Dashboard port")
	dashboardHost := dashboardCmd.String("host", "127.0.0.1", "Dashboard host")
	dashboardOutputDir := dashboardCmd.String("output-dir", ".traces", "Trace output directory")
	dashboardCmd.IntVar(dashboardPort, "tap-live-port", 8989, "Dashboard port")
	dashboardCmd.StringVar(dashboardHost, "tap-host", "127.0.0.1", "Dashboard host")
	dashboardCmd.StringVar(dashboardOutputDir, "tap-output-dir", ".traces", "Trace output directory")

	// Update subcommand
	updateCmd := flag.NewFlagSet("update", flag.ExitOnError)

	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "export":
			exportCmd.Parse(os.Args[2:])
			return exportMain(exportCmd.Args(), *exportOutput, *exportFormat)
		case "trust-ca":
			trustCACmd.Parse(os.Args[2:])
			return trustCAMain()
		case "dashboard":
			dashboardCmd.Parse(os.Args[2:])
			return dashboardMain(*dashboardPort, *dashboardHost, *dashboardOutputDir)
		case "update":
			updateCmd.Parse(os.Args[2:])
			return updateMain()
		}
	}

	flags.Parse(os.Args[1:])
	if tapNoLive {
		tapLive = false
	}

	// Resolve client config
	cfg, ok := client.ClientConfigs[tapClient]
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown client '%s'. Supported: claude, codex, kimi, gemini, opencode, hermes, cursor, pi, qoder, agy\n", tapClient)
		return 1
	}

	// Resolve proxy mode
	proxyMode := tapProxyMode
	if proxyMode == "" {
		proxyMode = cfg.DefaultProxyMode
	}

	// Resolve target
	target := tapTarget
	if target == "" {
		target = client.DetectTarget(tapClient, cfg)
	}

	// Resolve extra allowed path prefixes
	var extraPrefixes []string
	if extraAllowedPaths != "" {
		extraPrefixes = strings.Split(extraAllowedPaths, ",")
	}

	// Create output directory
	outputDir := tapOutputDir
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create output directory: %v\n", err)
		return 1
	}

	// Generate trace file paths
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("150405")
	ts := now.Format("20060102_150405")
	dateDir := filepath.Join(outputDir, dateStr)
	os.MkdirAll(dateDir, 0755)

	tracePath := filepath.Join(dateDir, fmt.Sprintf("trace_%s.jsonl", timeStr))
	logPath := filepath.Join(dateDir, fmt.Sprintf("trace_%s.log", timeStr))

	// Setup log file
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create log file: %v\n", err)
		return 1
	}
	defer logFile.Close()

	logOutput := io.Writer(logFile)
	if tapConsoleLog {
		logOutput = io.MultiWriter(logFile, os.Stderr)
	}
	log.SetOutput(logOutput)
	log.SetFlags(log.LstdFlags)

	// Setup trace writer
	traceMetadata := map[string]string{
		"client":     tapClient,
		"proxy_mode": proxyMode,
	}
	writer, err := trace.NewWriter(tracePath, traceMetadata)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create trace writer: %v\n", err)
		return 1
	}
	defer writer.Close()

	// Determine CA cert path for forward proxy
	var caCertPath, caKeyPath string
	if proxyMode == "forward" {
		caCertPath, caKeyPath, err = ca.EnsureCA(defaultCADir())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to setup CA: %v\n", err)
			return 1
		}
	}

	// Start live viewer if enabled
	var liveServer *live.Server
	if tapLive {
		liveServer = live.NewServer(tracePath, tapLivePort, tapHost, outputDir)
		actualLivePort, err := liveServer.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Live viewer start warning: %v\n", err)
		} else {
			fmt.Printf("Live viewer: http://%s:%d\n", tapHost, actualLivePort)
			if !tapNoOpen {
				go openBrowser(fmt.Sprintf("http://%s:%d", tapHost, actualLivePort))
			}
			writer.SetBroadcaster(liveServer)
		}
		defer liveServer.Stop()
	}

	// Print startup banner
	fmt.Printf("claude-tap v%s proxy on http://%s:%d\n", version, tapHost, tapPort)
	if proxyMode == "forward" {
		fmt.Printf("   Mode: forward (MITM)\n")
		fmt.Printf("   CA cert: %s\n", caCertPath)
	} else {
		fmt.Printf("   Mode: reverse\n")
	}
	fmt.Printf("   Upstream: %s\n", target)
	fmt.Printf("Trace file: %s\n", tracePath)
	fmt.Printf("   Proxy log: %s\n", logPath)
	if tapConsoleLog {
		fmt.Printf("   Console log: enabled\n")
	}

	// Start proxy server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var actualPort int

	if proxyMode == "forward" {
		// Forward proxy (MITM)
		caObj, err := ca.NewCA(caCertPath, caKeyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load CA: %v\n", err)
			return 1
		}

		fwdServer := proxy.NewForwardProxyServer(tapHost, tapPort, caObj, writer, target, cfg.ForwardBaseURLAllowedPathPrefixes)
		actualPort, err = fwdServer.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start forward proxy: %v\n", err)
			return 1
		}
		defer fwdServer.Stop()

		fmt.Printf("claude-tap v%s forward proxy on http://%s:%d\n", version, tapHost, actualPort)
	} else {
		// Reverse proxy
		reverseCtx := &proxy.ReverseProxyContext{
			TargetURL:         target,
			Writer:            writer,
			ExtraPathPrefixes: extraPrefixes,
			StripPathPrefix:   cfg.ReverseStripPathPrefix(target),
		}

		mux := http.NewServeMux()
		mux.Handle("/", proxy.ReverseProxyHandler(reverseCtx))

		server := &http.Server{
			Addr:    fmt.Sprintf("%s:%d", tapHost, tapPort),
			Handler: mux,
		}

		listener, err := netListen(tapHost, tapPort)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to bind: %v\n", err)
			return 1
		}
		actualPort = listener.Addr().(*net.TCPAddr).Port

		go server.Serve(listener)
		defer server.Shutdown(context.Background())

		fmt.Printf("claude-tap v%s listening on http://%s:%d\n", version, tapHost, actualPort)
	}

	// Spawn the client subprocess (unless --tap-no-launch)
	var clientProc *exec.Cmd
	if !tapNoLaunch {
		resolvedCmd, err := cfg.ResolveCmd()
		if err != nil {
			fmt.Fprintln(os.Stderr, cfg.MissingHelp())
			return 1
		}

		_, envSlice := cfg.BuildEnv(actualPort, proxyMode, caCertPath)
		cmdArgs := cfg.BuildArgs(flags.Args(), actualPort, proxyMode, caCertPath)

		clientProc = exec.Command(resolvedCmd, cmdArgs...)
		clientProc.Env = envSlice
		clientProc.Stdin = os.Stdin
		clientProc.Stdout = os.Stdout
		clientProc.Stderr = os.Stderr

		fmt.Printf("\nStarting %s: %s\n", cfg.Label, strings.Join(append([]string{cfg.Cmd}, cmdArgs...), " "))
		if proxyMode == "forward" {
			fmt.Printf("   HTTPS_PROXY=http://127.0.0.1:%d\n", actualPort)
		} else {
			for envKey, baseURL := range cfg.ReverseBaseURLEnvMap(actualPort) {
				fmt.Printf("   %s=%s\n", envKey, baseURL)
			}
		}
		fmt.Println()

		if err := clientProc.Start(); err != nil {
			log.Printf("Failed to start %s: %v", cfg.Cmd, err)
			fmt.Fprintf(os.Stderr, "Failed to start %s: %v\n", cfg.Cmd, err)
			return 1
		}
	}

	// Wait for signals or client exit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	clientDone := make(chan error, 1)
	if clientProc != nil {
		go func() {
			clientDone <- clientProc.Wait()
		}()
	}

	// Signal handling
	sigintCount := 0
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGINT {
				sigintCount++
				if sigintCount >= 2 {
					fmt.Fprintf(os.Stderr, "\nForce quitting...\n")
					if clientProc != nil {
						clientProc.Process.Kill()
					}
					cancel()
					goto done
				}
				fmt.Fprintf(os.Stderr, "\nPress Ctrl+C again to force quit\n")
				if clientProc != nil {
					clientProc.Process.Signal(os.Interrupt)
				}
			} else {
				if clientProc != nil {
					clientProc.Process.Kill()
				}
				cancel()
				goto done
			}
		case <-clientDone:
			// Client exited normally
			goto done
		case <-ctx.Done():
			goto done
		}
	}

done:
	_ = ctx

	// Generate HTML viewer
	htmlPath := tracePath[:len(tracePath)-len(".jsonl")] + ".html"
	generateHTMLViewer(tracePath, htmlPath, writer)

	// Register trace in manifest and cleanup old traces
	files := []string{
		history.RelPosix(tracePath, outputDir),
		history.RelPosix(logPath, outputDir),
		history.RelPosix(htmlPath, outputDir),
	}

	traceMetadata["client"] = tapClient
	traceMetadata["proxy_mode"] = proxyMode
	history.RegisterTrace(outputDir, ts, files, traceMetadata)
	history.CleanupTraces(outputDir, tapMaxTraces)

	// Print summary
	summary := writer.Summary()
	fmt.Println()
	fmt.Println("Trace summary:")
	fmt.Printf("   API calls: %v\n", summary["api_calls"])
	fmt.Printf("   Trace: %s\n", tracePath)
	fmt.Printf("   Log:   %s\n", logPath)
	fmt.Printf("   View:  %s\n", htmlPath)

	// Open viewer in browser
	if !tapNoOpen {
		go openBrowser("file://" + htmlPath)
	}

	return 0
}

func exportMain(args []string, output, format string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-tap export <trace.jsonl> [-o output] [--format markdown|json|html]")
		return 1
	}

	inputPath := args[0]
	data, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read trace file: %v\n", err)
		return 1
	}

	var records []map[string]interface{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]interface{}
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}

	var out io.Writer = os.Stdout
	if output != "" {
		f, err := os.Create(output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create output: %v\n", err)
			return 1
		}
		defer f.Close()
		out = f
	}

	switch format {
	case "markdown":
		writeMarkdownExport(out, records)
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(records)
	case "html":
		fmt.Fprintln(out, "<html><body><pre>")
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(records)
		fmt.Fprintln(out, "</pre></body></html>")
	default:
		fmt.Fprintf(os.Stderr, "Unknown format: %s\n", format)
		return 1
	}

	return 0
}

func writeMarkdownExport(w io.Writer, records []map[string]interface{}) {
	fmt.Fprintf(w, "# Trace Export\n\n")
	fmt.Fprintf(w, "**Total API calls:** %d\n\n", len(records))

	for _, record := range records {
		req, _ := record["request"].(map[string]interface{})
		resp, _ := record["response"].(map[string]interface{})
		reqBody, _ := req["body"].(map[string]interface{})
		respBody, _ := resp["body"].(map[string]interface{})

		turn := record["turn"]
		model := ""
		if reqBody != nil {
			if m, ok := reqBody["model"].(string); ok {
				model = m
			}
		}

		fmt.Fprintf(w, "## Turn %v (%s)\n\n", turn, model)
		fmt.Fprintf(w, "- Duration: %vms\n", record["duration_ms"])
		fmt.Fprintf(w, "- Status: %v\n", resp["status"])
		fmt.Fprintf(w, "- Path: %v %v\n", req["method"], req["path"])

		if reqBody != nil {
			fmt.Fprintf(w, "\n### Request Body\n\n```json\n")
			data, _ := json.MarshalIndent(reqBody, "", "  ")
			w.Write(data)
			fmt.Fprintf(w, "\n```\n")
		}

		if respBody != nil {
			fmt.Fprintf(w, "\n### Response Body\n\n```json\n")
			data, _ := json.MarshalIndent(respBody, "", "  ")
			w.Write(data)
			fmt.Fprintf(w, "\n```\n")
		}
		fmt.Fprintf(w, "\n---\n\n")
	}
}

func trustCAMain() int {
	caCertPath, caKeyPath, err := ca.EnsureCA(defaultCADir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup CA: %v\n", err)
		return 1
	}
	fmt.Printf("CA certificate: %s\n", caCertPath)
	fmt.Printf("CA private key:  %s\n", caKeyPath)
	fmt.Println("Import the CA certificate into your OS or browser trust store if your client requires trusted MITM certificates.")
	return 0
}

func dashboardMain(port int, host, outputDir string) int {
	// Placeholder trace path for dashboard mode
	tracePath := filepath.Join(outputDir, ".dashboard", "current.jsonl")
	os.MkdirAll(filepath.Dir(tracePath), 0755)

	server := live.NewServer(tracePath, port, host, outputDir)
	actualPort, err := server.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start dashboard: %v\n", err)
		return 1
	}

	fmt.Printf("Dashboard: http://%s:%d\n", host, actualPort)
	openBrowser(fmt.Sprintf("http://%s:%d/dashboard", host, actualPort))

	// Wait for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	server.Stop()
	return 0
}

func updateMain() int {
	fmt.Println("Update is not implemented in the Go port yet. Upgrade with: go install github.com/wywang792/claude-tap-go/cmd/claude-tap@latest")
	return 0
}

func generateHTMLViewer(tracePath, htmlPath string, writer *trace.Writer) {
	data, err := os.ReadFile(tracePath)
	if err != nil {
		return
	}

	var records []map[string]interface{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]interface{}
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}

	html := live.GenerateViewerHTMLContent(records, tracePath, htmlPath)
	os.WriteFile(htmlPath, []byte(html), 0644)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		cmd = exec.Command("wslview", url)
	} else {
		switch {
		case strings.Contains(os.Getenv("OS"), "Windows"):
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		case runtime.GOOS == "darwin":
			cmd = exec.Command("open", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
	}
	if cmd == nil {
		return
	}
	cmd.Start()
}

func defaultCADir() string {
	if home := os.Getenv("USERPROFILE"); home != "" {
		return filepath.Join(home, ".claude-tap")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".claude-tap")
	}
	return ".claude-tap"
}

func netListen(host string, port int) (netListener, error) {
	return net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
}

type netListener = net.Listener
