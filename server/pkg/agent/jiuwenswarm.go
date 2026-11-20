package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const jiuwenSwarmBinary = "jiuwenswarm-acp"

var jiuwenSwarmBlockedArgs = map[string]blockedArgMode{
	"acp":       blockedStandalone,
	"--version": blockedStandalone,
}

type jiuwenSwarmBackend struct {
	cfg Config
}

func (b *jiuwenSwarmBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	servers, err := buildACPMcpServers(opts.McpConfig, b.cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("jiuwenswarm: invalid mcp_config: %w", err)
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	path := b.cfg.ExecutablePath
	if path == "" {
		path = jiuwenSwarmBinary
	}
	runCtx, cancel := runContext(ctx, opts.Timeout)
	procCtx, stopProcess := context.WithCancel(context.WithoutCancel(runCtx))
	args := filterCustomArgs(opts.CustomArgs, jiuwenSwarmBlockedArgs, b.cfg.Logger)
	cmd := b.cfg.commandAt(path).exec(procCtx, args...)
	cmd.Dir, cmd.Env = cwd, buildEnv(b.cfg.Env)
	hideAgentWindow(cmd)
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args))
	stderr := newStderrTail(newLogWriter(b.cfg.Logger, "[jiuwenswarm:stderr] "), 4096)
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		stopProcess()
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stdout.Close()
		cancel()
		stopProcess()
		return nil, err
	}
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		cancel()
		stopProcess()
		return nil, fmt.Errorf("start jiuwenswarm: %w", err)
	}

	messages := make(chan Message, 256)
	results := make(chan Result, 1)
	activity := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	promptDone := make(chan hermesPromptResult, 1)
	var streaming atomic.Bool
	var deliverable acpDeliverableTracker
	c := &hermesClient{
		cfg: b.cfg, stdin: stdin, pending: make(map[int]*pendingRPC),
		pendingTools:    make(map[string]*pendingToolCall),
		terminalEnabled: true, terminalCtx: runCtx, terminalCwd: cwd,
		terminalEnv: cmd.Env, terminals: make(map[string]*acpTerminal),
		onUsage:            opts.OnUsage,
		acceptNotification: func(string) bool { return streaming.Load() },
		onActivity: func() {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
		onMessage: func(msg Message) {
			deliverable.observe(msg)
			trySend(messages, msg)
		},
		onPromptDone: func(result hermesPromptResult) { promptDone <- result },
	}
	go func() {
		defer close(readerDone)
		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				c.handleLine(line)
			}
		}
		c.closeAllPending(errors.New("jiuwenswarm process exited"))
	}()

	go func() {
		defer close(messages)
		defer close(results)
		defer cancel()
		start := time.Now()
		result := Result{Status: "completed"}
		stopSetupWatcher := context.AfterFunc(runCtx, stopProcess)
		err := b.runSession(runCtx, c, cwd, servers, prompt, opts, &result, &streaming, stopSetupWatcher)
		stopSetupWatcher()
		if err == nil && runCtx.Err() == nil {
			waitForACPNotificationQuiescence(runCtx, activity, readerDone, acpNotificationQuietTime, time.Second)
		}
		_ = stdin.Close()
		select {
		case <-readerDone:
		case <-time.After(500 * time.Millisecond):
		}
		stopProcess()
		_ = cmd.Wait()
		<-readerDone
		releaseProcessGroup(cmd)
		for id := range c.terminals {
			if err := c.acpTerminalRelease(id); err != nil {
				b.cfg.Logger.Warn("jiuwenswarm terminal cleanup failed", "error", err)
			}
		}

		switch runCtx.Err() {
		case context.Canceled:
			result.Status, result.Error = "aborted", "execution cancelled"
		case context.DeadlineExceeded:
			result.Status, result.Error = "timeout", fmt.Sprintf("jiuwenswarm timed out after %s", opts.Timeout)
		default:
			if err != nil {
				result.Status = "failed"
				if result.Error == "" {
					result.Error = withAgentStderr(err.Error(), "jiuwenswarm", sanitizeAgentDiagnostic(stderr.Tail()))
				}
			}
		}
		select {
		case pr := <-promptDone:
			c.mergeUsage(pr.usage)
			if result.Status == "completed" {
				switch pr.stopReason {
				case "", "end_turn":
				case "cancelled":
					result.Status, result.Error = "aborted", "jiuwenswarm cancelled the prompt"
				default:
					result.Status, result.Error = "failed", "jiuwenswarm stopped: "+pr.stopReason
				}
			}
		default:
		}
		result.Output, _ = deliverable.result()
		result.Usage = c.liveUsage()
		result.DurationMs = time.Since(start).Milliseconds()
		results <- result
	}()
	return &Session{Messages: messages, Result: results, CleanupRequired: true}, nil
}

func (b *jiuwenSwarmBackend) runSession(ctx context.Context, c *hermesClient, cwd string, servers []any, prompt string, opts ExecOptions, result *Result, streaming *atomic.Bool, stopSetupWatcher func() bool) error {
	init, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"terminal": true,
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
		},
	})
	if err != nil {
		return fmt.Errorf("jiuwenswarm initialize: %w", err)
	}
	params := map[string]any{
		"cwd":        cwd,
		"mcpServers": filterACPMcpServersByCapability(servers, extractACPMcpCapabilities(init), "jiuwenswarm", b.cfg),
	}
	method := "session/new"
	if opts.ResumeSessionID != "" {
		method = jiuwenSwarmResumeMethod(init)
		if method == "" {
			method = "session/new"
		} else {
			params["sessionId"] = opts.ResumeSessionID
		}
	}
	setup, err := c.request(ctx, method, params)
	if err != nil {
		if method != "session/new" {
			result.Status, result.Error, result.ResumeRejected = classifyACPResumeFailure(ctx, "jiuwenswarm", method, err, opts.Timeout, b.cfg.Logger)
		}
		return fmt.Errorf("jiuwenswarm %s: %w", method, err)
	}
	sessionID := extractACPSessionID(setup)
	if method != "session/new" && sessionID == "" {
		sessionID = opts.ResumeSessionID
	}
	if sessionID == "" {
		return errors.New("jiuwenswarm session/new returned no session ID")
	}
	c.sessionID = sessionID
	currentModel := extractACPCurrentModelID(setup)
	if opts.Model != "" && opts.Model != currentModel && len(parseACPSessionNewModels(setup)) > 0 {
		var switched json.RawMessage
		switched, err = c.request(ctx, "session/set_model", map[string]string{"sessionId": sessionID, "modelId": opts.Model})
		if err != nil {
			result.ResumeRejected = method != "session/new" && isACPSessionNotFound(err)
			return fmt.Errorf("jiuwenswarm could not switch to model %q: %w", opts.Model, err)
		}
		currentModel = opts.Model
		if opts.ThinkingLevel != "" {
			setup, err = acpOptionsAfterModelSwitch(ctx, c.request, sessionID, opts.Model, setup, switched)
			if err != nil {
				b.cfg.Logger.Warn("jiuwenswarm could not refresh model options", "error", err)
			}
		}
	}
	applyACPEffortOption(ctx, c.request, "jiuwenswarm", b.cfg.Logger, sessionID, setup, opts.ThinkingLevel, true)
	c.usageMu.Lock()
	c.usageModel = currentModel
	c.usageMu.Unlock()

	// Let the gateway finish cancellation before stopping its stdio bridge.
	stopSetupWatcher()
	if err := ctx.Err(); err != nil {
		return err
	}
	if jiuwenSwarmResumeMethod(init) != "" {
		result.SessionID = sessionID
	}
	prompt = hermesTurnText(prompt, opts.ResumeExpected, method != "session/new" && sessionID == opts.ResumeSessionID, opts.ResumeContinuityNotice)
	if opts.SystemPrompt != "" {
		prompt = opts.SystemPrompt + "\n\n---\n\n" + prompt
	}
	streaming.Store(true)
	sent := make(chan struct{})
	response := make(chan error, 1)
	go func() {
		_, err := c.requestAndNotifySent(context.WithoutCancel(ctx), "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": prompt}},
		}, func() { close(sent) })
		response <- err
	}()
	select {
	case err := <-response:
		if method != "session/new" && isACPSessionNotFound(err) {
			result.SessionID, result.ResumeRejected = "", true
		}
		if err != nil {
			return fmt.Errorf("jiuwenswarm session/prompt: %w", err)
		}
		return nil
	case <-ctx.Done():
		select {
		case <-sent:
			go func() { _ = c.notify("session/cancel", map[string]string{"sessionId": sessionID}) }()
			select {
			case <-response:
			case <-time.After(2 * time.Second):
			}
		default:
		}
		return ctx.Err()
	}
}

func jiuwenSwarmResumeMethod(raw json.RawMessage) string {
	var init struct {
		Capabilities struct {
			LoadSession bool `json:"loadSession"`
			Session     struct {
				Resume *struct{} `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &init)
	if init.Capabilities.Session.Resume != nil {
		return "session/resume"
	}
	if init.Capabilities.LoadSession {
		return "session/load"
	}
	return ""
}
