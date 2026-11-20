//go:build !windows

package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJiuwenSwarmACP(t *testing.T) {
	tests := []struct {
		name, mode, result string
	}{
		{"PoC playback", "poc", "completed"},
		{"negotiated capabilities", "capable", "completed"},
		{"persistent session load", "load", "completed"},
		{"expired session", "expired", "failed"},
		{"gateway unreachable", "offline", "failed"},
		{"cancel", "cancel", "aborted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture, err := filepath.Abs("testdata/jiuwenswarm/poc.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			transcript := filepath.Join(dir, "requests")
			binary := filepath.Join(dir, jiuwenSwarmBinary)
			writeTestExecutable(t, binary, []byte(jiuwenSwarmTestScript))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := ExecOptions{Cwd: dir, Timeout: 10 * time.Second, Model: "test-model",
				ThinkingLevel: "high", ResumeSessionID: "swarm-test",
				CustomArgs: []string{"acp", "--version"},
				McpConfig:  []byte(`{"mcpServers":{"local":{"command":"echo"},"remote":{"type":"http","url":"https://example.test/mcp"}}}`),
			}
			b := jiuwenSwarmBackend{cfg: Config{ExecutablePath: binary, Logger: slog.Default(), Env: map[string]string{
				"SWARM_FIXTURE": fixture, "SWARM_REQUESTS": transcript, "SWARM_MODE": tt.mode,
			}}}
			session, err := b.Execute(ctx, "test", opts)
			if err != nil {
				t.Fatal(err)
			}
			var toolUse, toolResult bool
			for msg := range session.Messages {
				switch msg.Type {
				case MessageToolUse:
					toolUse = msg.CallID == "poc-edit" && msg.Input["file_path"] == "acp-poc-marker.txt"
				case MessageToolResult:
					toolResult = msg.CallID == "poc-edit" && strings.Contains(msg.Output, "POC_FILE_OK")
				case MessageText:
					if tt.mode == "cancel" {
						cancel()
					}
				}
			}
			res := <-session.Result
			if res.Status != tt.result {
				t.Fatalf("result = %+v", res)
			}
			requests, err := os.ReadFile(transcript)
			if err != nil {
				t.Fatal(err)
			}
			var want, absent []string
			switch tt.mode {
			case "poc":
				if !toolUse || !toolResult || res.Output != "done" || res.SessionID != "" {
					t.Fatalf("replay result = %+v, tool use = %v, tool result = %v", res, toolUse, toolResult)
				}
				want = []string{"session/new", `"name":"local"`, `"terminal":true`, `"readTextFile":false`}
				absent = []string{"session/resume", "session/load", "session/set_model", "session/set_config_option", "example.test"}
			case "capable":
				want = []string{"session/resume", "session/set_model", "session/set_config_option", "example.test"}
				if res.SessionID != "swarm-test" {
					t.Fatalf("session id = %q", res.SessionID)
				}
			case "load":
				want = []string{"session/load", "session/set_model", "session/prompt"}
				absent = []string{"session/new", "session/set_config_option"}
				if res.SessionID != "swarm-test" {
					t.Fatalf("session id = %q", res.SessionID)
				}
			case "expired":
				want = []string{"session/load"}
				absent = []string{"session/new", "session/prompt"}
				if !res.ResumeRejected || res.SessionID != "" {
					t.Fatalf("expected daemon fallback signal, got %+v", res)
				}
			case "offline":
				if !strings.Contains(res.Error, "JiuwenSwarm Gateway unreachable") {
					t.Fatalf("gateway diagnostic missing: %+v", res)
				}
				absent = []string{"session/new", "session/prompt"}
			case "cancel":
				want = []string{"session/cancel"}
			}
			for _, line := range want {
				if !strings.Contains(string(requests), line) {
					t.Errorf("missing %q in %s", line, requests)
				}
			}
			for _, line := range absent {
				if strings.Contains(string(requests), line) {
					t.Errorf("unexpected %q in %s", line, requests)
				}
			}
		})
	}
}

func TestJiuwenSwarmResumeMethod(t *testing.T) {
	for _, tt := range []struct{ init, method string }{
		{`{"agentCapabilities":{"loadSession":false}}`, ""},
		{`{"agentCapabilities":{"loadSession":true}}`, "session/load"},
		{`{"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}`, "session/resume"},
	} {
		if got := jiuwenSwarmResumeMethod([]byte(tt.init)); got != tt.method {
			t.Errorf("resume method for %s = %q, want %q", tt.init, got, tt.method)
		}
	}
}

func TestJiuwenSwarmModelDiscovery(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, jiuwenSwarmBinary)
	writeTestExecutable(t, binary, []byte(jiuwenSwarmTestScript))
	t.Setenv("SWARM_MODE", "load")
	t.Setenv("SWARM_REQUESTS", filepath.Join(dir, "requests"))
	catalog, err := ListModels(context.Background(), "jiuwenswarm", Command{Path: binary})
	if err != nil || !catalog.Verified() || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %+v, err = %v", catalog, err)
	}
	if first, second := catalog.Models[0], catalog.Models[1]; first.ID != "default" || !first.Default || second.ID != "test-model" {
		t.Fatalf("unexpected models: %+v", catalog.Models)
	}
}

func TestJiuwenSwarmVersion(t *testing.T) {
	binary, record := fakeHermesForVersionProbe(t, `[ "$*" = "--version" ] && echo "0.2.9b5"`)
	got, err := DetectVersion(context.Background(), Command{Path: binary}.ForVersionProbe("jiuwenswarm"))
	if err != nil || !strings.HasPrefix(got, "0.2.9") {
		t.Fatalf("version = %q, err = %v", got, err)
	}
	if args := readProbeInvocations(t, record); len(args) != 1 || args[0] != "--version" {
		t.Fatalf("version argv = %q", args)
	}
}

const jiuwenSwarmTestScript = `#!/bin/sh
[ "$#" -eq 0 ] || exit 2
emit() { printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$1" "$2"; }
models='{"sessionId":"swarm-test","models":{"currentModelId":"default","availableModels":[{"modelId":"default","name":"Default"},{"modelId":"test-model","name":"Test"}]}}'
options='{"configOptions":[{"id":"effort","category":"thought_level","currentValue":"low","options":[{"value":"low"},{"value":"high"}]}]}'
while IFS= read -r req; do
  printf '%s\n' "$req" >> "$SWARM_REQUESTS"
  id=$(printf '%s\n' "$req" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$req" in
    *'"initialize"'*)
      case "$SWARM_MODE" in
        offline) printf 'JiuwenSwarm Gateway unreachable: ws://127.0.0.1:1/ws\n' >&2; exit 1 ;;
        capable) emit "$id" '{"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"resume":{}},"mcpCapabilities":{"http":true}}}' ;;
        load|expired) emit "$id" '{"agentCapabilities":{"loadSession":true}}' ;;
        *) emit "$id" "$(sed -n '1p' "$SWARM_FIXTURE")" ;;
      esac ;;
    *'"session/new"'*)
      if [ "$SWARM_MODE" = load ]; then emit "$id" "$models"; else emit "$id" '{"sessionId":"swarm-test","configOptions":[]}'; fi ;;
    *'"session/load"'*)
      if [ "$SWARM_MODE" = expired ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"Session not found or expired"}}\n' "$id"
      else
        emit "$id" "$models"
      fi ;;
    *'"session/resume"'*) emit "$id" "$models" ;;
    *'"session/set_model"'*)
      if [ "$SWARM_MODE" = load ]; then emit "$id" '{}'; else emit "$id" "$options"; fi ;;
    *'"session/set_config_option"'*) emit "$id" "$(printf '%s' "$options" | sed 's/"currentValue":"low"/"currentValue":"high"/')" ;;
    *'"session/prompt"'*)
      if [ "$SWARM_MODE" = poc ]; then sed -n '2,3p' "$SWARM_FIXTURE"; fi
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"swarm-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}'
      if [ "$SWARM_MODE" = cancel ]; then prompt_id="$id"; else emit "$id" '{"stopReason":"end_turn"}'; fi ;;
    *'"session/cancel"'*) emit "$prompt_id" "$(sed -n '4p' "$SWARM_FIXTURE")" ;;
  esac
done
`
