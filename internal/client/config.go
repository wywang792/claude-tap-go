package client

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ClientConfig holds per-client configuration for supported AI CLI tools.
type ClientConfig struct {
	Cmd                                 string   // CLI binary name
	Label                               string   // Human-readable label
	InstallURL                          string   // Documentation/install link
	BaseURLEnv                          string   // Primary env var for base URL
	BaseURLSuffix                       string   // Suffix appended to http://127.0.0.1:{port}
	DefaultTarget                       string   // Upstream API URL
	ExtraBaseURLEnvs                    []string // Additional env vars for base URL
	NestingEnvKeys                      []string // Env vars to clear before launch
	InjectSettingsEnv                   bool     // Inject env via --settings JSON
	BaseURLConfigKey                    string   // CLI config override key
	StripPathPrefix                     string   // Path prefix to strip in reverse proxy
	StripPathPrefixUnlessTargetContains []string
	DefaultProxyMode                    string // "reverse" or "forward"
	AutoTrustCAMacOS                    bool
	ForwardBaseURLEnvs                  []string
	ForwardBaseURLAllowedPathPrefixes   []string
}

const codexChatGPTTarget = "https://chatgpt.com/backend-api/codex"

// ClientConfigs maps client name to configuration.
var ClientConfigs = map[string]ClientConfig{
	"claude": {
		Cmd:               "claude",
		Label:             "Claude Code",
		InstallURL:        "https://docs.anthropic.com/en/docs/claude-code",
		BaseURLEnv:        "ANTHROPIC_BASE_URL",
		BaseURLSuffix:     "",
		DefaultTarget:     "https://api.anthropic.com",
		NestingEnvKeys:    []string{"CLAUDECODE", "CLAUDE_CODE_SSE_PORT"},
		InjectSettingsEnv: true,
		DefaultProxyMode:  "reverse",
	},
	"codex": {
		Cmd:                                 "codex",
		Label:                               "Codex CLI",
		InstallURL:                          "https://github.com/openai/codex",
		BaseURLEnv:                          "OPENAI_BASE_URL",
		BaseURLSuffix:                       "/v1",
		DefaultTarget:                       "https://api.openai.com",
		BaseURLConfigKey:                    "openai_base_url",
		StripPathPrefix:                     "/v1",
		StripPathPrefixUnlessTargetContains: []string{"api.openai.com"},
		DefaultProxyMode:                    "reverse",
	},
	"kimi": {
		Cmd:              "kimi",
		Label:            "Kimi Code CLI",
		InstallURL:       "https://github.com/MoonshotAI/kimi-cli",
		BaseURLEnv:       "KIMI_BASE_URL",
		BaseURLSuffix:    "",
		DefaultTarget:    "https://api.kimi.com/coding/v1",
		DefaultProxyMode: "reverse",
	},
	"gemini": {
		Cmd:              "gemini",
		Label:            "Gemini CLI",
		InstallURL:       "https://github.com/google-gemini/gemini-cli",
		BaseURLEnv:       "GOOGLE_GEMINI_BASE_URL",
		ExtraBaseURLEnvs: []string{"GOOGLE_VERTEX_BASE_URL"},
		BaseURLSuffix:    "",
		DefaultTarget:    "https://generativelanguage.googleapis.com",
		DefaultProxyMode: "forward",
	},
	"opencode": {
		Cmd:              "opencode",
		Label:            "OpenCode",
		InstallURL:       "https://opencode.ai/docs/",
		BaseURLEnv:       "ANTHROPIC_BASE_URL",
		BaseURLSuffix:    "",
		DefaultTarget:    "https://api.anthropic.com",
		DefaultProxyMode: "forward",
	},
	"hermes": {
		Cmd:              "hermes",
		Label:            "Hermes Agent",
		InstallURL:       "https://github.com/NousResearch/hermes-agent",
		BaseURLEnv:       "OPENAI_BASE_URL",
		BaseURLSuffix:    "/v1",
		DefaultTarget:    "https://api.openai.com",
		DefaultProxyMode: "forward",
	},
	"cursor": {
		Cmd:              "cursor-agent",
		Label:            "Cursor CLI",
		InstallURL:       "https://cursor.com/cli",
		BaseURLEnv:       "CURSOR_BASE_URL",
		BaseURLSuffix:    "",
		DefaultTarget:    "https://api2.cursor.sh",
		DefaultProxyMode: "forward",
	},
	"pi": {
		Cmd:              "pi",
		Label:            "Pi",
		InstallURL:       "https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent",
		BaseURLEnv:       "OPENAI_BASE_URL",
		BaseURLSuffix:    "/v1",
		DefaultTarget:    "https://api.openai.com",
		DefaultProxyMode: "forward",
	},
	"qoder": {
		Cmd:              "qodercli",
		Label:            "Qoder CLI",
		InstallURL:       "https://qoder.com/cli",
		BaseURLEnv:       "QODER_BASE_URL",
		BaseURLSuffix:    "",
		DefaultTarget:    "https://api2.qoder.sh",
		DefaultProxyMode: "forward",
	},
	"agy": {
		Cmd:                               "agy",
		Label:                             "Antigravity CLI",
		InstallURL:                        "https://antigravity.google/product/antigravity-cli",
		BaseURLEnv:                        "CLOUD_CODE_URL",
		BaseURLSuffix:                     "",
		DefaultTarget:                     "https://daily-cloudcode-pa.googleapis.com",
		DefaultProxyMode:                  "forward",
		AutoTrustCAMacOS:                  true,
		ForwardBaseURLEnvs:                []string{"CLOUD_CODE_URL"},
		ForwardBaseURLAllowedPathPrefixes: []string{"/v1internal"},
	},
}

// ReverseBaseURL returns the reverse proxy URL for a client at the given port.
func (c ClientConfig) ReverseBaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, c.BaseURLSuffix)
}

// ReverseBaseURLEnvs returns the env var keys that need the reverse base URL.
func (c ClientConfig) ReverseBaseURLEnvs() []string {
	seen := make(map[string]bool)
	var result []string
	for _, key := range append([]string{c.BaseURLEnv}, c.ExtraBaseURLEnvs...) {
		if !seen[key] {
			seen[key] = true
			result = append(result, key)
		}
	}
	return result
}

// ReverseBaseURLEnvMap returns a map of env var keys to their reverse proxy base URLs.
func (c ClientConfig) ReverseBaseURLEnvMap(port int) map[string]string {
	baseURL := c.ReverseBaseURL(port)
	result := make(map[string]string)
	for _, key := range c.ReverseBaseURLEnvs() {
		result[key] = baseURL
	}
	return result
}

// ReverseStripPathPrefix returns the path prefix to remove for a selected target.
func (c ClientConfig) ReverseStripPathPrefix(target string) string {
	if c.StripPathPrefix == "" {
		return ""
	}
	for _, marker := range c.StripPathPrefixUnlessTargetContains {
		if strings.Contains(target, marker) {
			return ""
		}
	}
	return c.StripPathPrefix
}

// DetectTarget returns the upstream target the wrapped client would normally use.
func DetectTarget(clientName string, cfg ClientConfig) string {
	switch clientName {
	case "claude":
		return detectClaudeTarget(cfg)
	case "codex":
		return detectCodexTarget(cfg)
	default:
		return cfg.DefaultTarget
	}
}

func detectClaudeTarget(cfg ClientConfig) string {
	if target := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")); target != "" {
		return target
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(".", ".claude", "settings.local.json"),
		filepath.Join(".", ".claude", "settings.json"),
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".claude", "settings.json"))
	}

	for _, path := range candidates {
		if target := readSettingsEnvBaseURL(path, cfg.BaseURLEnv); target != "" {
			return target
		}
	}
	return cfg.DefaultTarget
}

func readSettingsEnvBaseURL(path, envKey string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}
	return strings.TrimSpace(settings.Env[envKey])
}

func detectCodexTarget(cfg ClientConfig) string {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if codexHome == "" {
		return cfg.DefaultTarget
	}

	data, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		return cfg.DefaultTarget
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return cfg.DefaultTarget
	}
	if auth.AuthMode == "chatgpt" {
		return codexChatGPTTarget
	}
	return cfg.DefaultTarget
}

// MissingHelp returns a user-friendly error message when the CLI command is not found.
func (c ClientConfig) MissingHelp() string {
	return fmt.Sprintf("\nError: '%s' command not found in PATH.\nPlease install %s first: %s\n", c.Cmd, c.Label, c.InstallURL)
}

// ResolveCmd resolves the CLI command using PATH lookup (handles .cmd/.bat on Windows).
func (c ClientConfig) ResolveCmd() (string, error) {
	resolved, err := exec.LookPath(c.Cmd)
	if err != nil {
		return "", fmt.Errorf("'%s' not found in PATH", c.Cmd)
	}
	return resolved, nil
}

// BuildEnv builds the environment variables for launching the client.
func (c ClientConfig) BuildEnv(port int, proxyMode string, caCertPath string) (map[string]string, []string) {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			env[e[:idx]] = e[idx+1:]
		}
	}

	if proxyMode == "forward" {
		proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
		env["ALL_PROXY"] = proxyURL
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["all_proxy"] = proxyURL
		extendNoProxy(env, []string{"localhost", "127.0.0.1", "::1"})
		for _, envKey := range c.ForwardBaseURLEnvs {
			env[envKey] = c.ReverseBaseURL(port)
		}
		if caCertPath != "" {
			env["NODE_EXTRA_CA_CERTS"] = caCertPath
			env["SSL_CERT_FILE"] = caCertPath
			env["CODEX_CA_CERTIFICATE"] = caCertPath
			env["REQUESTS_CA_BUNDLE"] = caCertPath
		}
	} else {
		reverseEnv := c.ReverseBaseURLEnvMap(port)
		for k, v := range reverseEnv {
			env[k] = v
		}
		env["NO_PROXY"] = "127.0.0.1"
		env["no_proxy"] = "127.0.0.1"
	}

	for _, key := range c.NestingEnvKeys {
		delete(env, key)
	}

	// Build env slice for exec.Cmd
	envSlice := make([]string, 0, len(env))
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	return env, envSlice
}

// BuildArgs builds the command-line arguments for the client, including --settings injection.
func (c ClientConfig) BuildArgs(extraArgs []string, port int, proxyMode string, caCertPath string) []string {
	var args []string

	if c.InjectSettingsEnv {
		settingsEnv := make(map[string]string)
		if proxyMode == "forward" {
			proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			settingsEnv["HTTP_PROXY"] = proxyURL
			settingsEnv["HTTPS_PROXY"] = proxyURL
			settingsEnv["ALL_PROXY"] = proxyURL
			settingsEnv["http_proxy"] = proxyURL
			settingsEnv["https_proxy"] = proxyURL
			settingsEnv["all_proxy"] = proxyURL
			if caCertPath != "" {
				settingsEnv["NODE_EXTRA_CA_CERTS"] = caCertPath
			}
		} else {
			for k, v := range c.ReverseBaseURLEnvMap(port) {
				settingsEnv[k] = v
			}
		}
		if !hasSettingsArg(extraArgs) {
			payload, _ := json.Marshal(map[string]interface{}{"env": settingsEnv})
			args = append(args, "--settings", string(payload))
		}
	}

	if c.BaseURLConfigKey != "" && !hasConfigOverride(extraArgs, c.BaseURLConfigKey) {
		baseURL := c.ReverseBaseURL(port)
		args = append(args, "-c", fmt.Sprintf(`%s="%s"`, c.BaseURLConfigKey, baseURL))
	}

	args = append(args, extraArgs...)
	return maybeRewriteHermesGatewayStart(c.Cmd, args)
}

func extendNoProxy(env map[string]string, values []string) {
	var existing []string
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		for _, part := range strings.Split(env[key], ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				existing = append(existing, part)
			}
		}
	}
	seen := make(map[string]bool)
	var merged []string
	for _, value := range append(existing, values...) {
		lowered := strings.ToLower(value)
		if seen[lowered] {
			continue
		}
		seen[lowered] = true
		merged = append(merged, value)
	}
	noProxy := strings.Join(merged, ",")
	env["NO_PROXY"] = noProxy
	env["no_proxy"] = noProxy
}

func maybeRewriteHermesGatewayStart(cmd string, args []string) []string {
	if cmd != "hermes" {
		return args
	}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--profile" || arg == "-p":
			if i+1 < len(args) {
				i += 2
				continue
			}
		case strings.HasPrefix(arg, "--profile=") || strings.HasPrefix(arg, "-p="):
			i++
			continue
		case arg == "--ignore-user-config" || arg == "--accept-hooks":
			i++
			continue
		}
		break
	}
	if i+1 < len(args) && args[i] == "gateway" && args[i+1] == "start" {
		rewritten := append([]string{}, args[:i]...)
		rewritten = append(rewritten, "gateway", "run")
		return append(rewritten, args[i+2:]...)
	}
	return args
}

func hasSettingsArg(args []string) bool {
	for _, a := range args {
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			return true
		}
	}
	return false
}

func hasConfigOverride(args []string, key string) bool {
	prefix := key + "="
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-c" || arg == "--config") && i+1 < len(args) && strings.HasPrefix(args[i+1], prefix) {
			return true
		}
		if strings.HasPrefix(arg, "--config=") {
			value := arg[len("--config="):]
			if strings.HasPrefix(value, prefix) {
				return true
			}
		}
	}
	return false
}
