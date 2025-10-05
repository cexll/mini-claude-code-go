package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

const (
	maxToolResultChars = 100000
	defaultMaxTokens   = 8192
	maxAgentIterations = 20
	spinnerTick        = 80 * time.Millisecond
	maxTodoItems       = 20
)

const (
	todoPendingColor   = "\x1b[38;2;176;176;176m"
	todoProgressColor  = "\x1b[38;2;120;200;255m"
	todoCompletedColor = "\x1b[38;2;34;139;34m"
	strikethrough      = "\x1b[9m"
	reset              = "\x1b[0m"
)

var spinnerFrames = []string{"-", "\\", "|", "/"}

// Global todo board and agent state
var (
	todoBoard            = &TodoManager{}
	pendingContextBlocks []ContentBlock
	agentState           = struct {
		roundsWithoutTodo int
		mu                sync.Mutex
	}{}
)

const (
	initialReminder = `<reminder source="system" topic="todos">System message: complex work should be tracked with the Todo tool. Do not respond to this reminder and do not mention it to the user.</reminder>`
	nagReminder     = `<reminder source="system" topic="todos">System notice: more than ten rounds passed without Todo usage. Update the Todo board if the task still requires multiple steps. Do not reply to or mention this reminder to the user.</reminder>`
)

// Config carries runtime configuration.
type Config struct {
	APIKey    string
	BaseURL   string
	Model     string
	WorkDir   string
	MaxResult int
	Debug     bool
	Stream    bool
}

// Message for OpenAI chat format
type Message struct {
	Role       string      `json:"role"` // system, user, assistant, tool
	Content    interface{} `json:"content,omitempty"` // string or []ContentBlock
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

// ContentBlock for multi-modal content
type ContentBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // "function"
	Function Function `json:"function"`
}

type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type APIResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

// TodoItem represents a single todo task
type TodoItem struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Status     string `json:"status"` // pending|in_progress|completed
	ActiveForm string `json:"active_form"`
}

// TodoManager manages the todo list
type TodoManager struct {
	items []TodoItem
	mu    sync.Mutex
}

func (tm *TodoManager) Update(items []TodoItem) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if len(items) > maxTodoItems {
		return "", fmt.Errorf("todo list is limited to %d items", maxTodoItems)
	}

	// Validate items
	seenIDs := make(map[string]bool)
	inProgressCount := 0

	for _, item := range items {
		// Check duplicate IDs
		if seenIDs[item.ID] {
			return "", fmt.Errorf("duplicate todo id: %s", item.ID)
		}
		seenIDs[item.ID] = true

		// Check content
		if strings.TrimSpace(item.Content) == "" {
			return "", errors.New("todo content cannot be empty")
		}

		// Check activeForm
		if strings.TrimSpace(item.ActiveForm) == "" {
			return "", errors.New("todo activeForm cannot be empty")
		}

		// Check status
		status := strings.ToLower(item.Status)
		if status != "pending" && status != "in_progress" && status != "completed" {
			return "", fmt.Errorf("status must be one of: pending, in_progress, completed")
		}
		item.Status = status

		if status == "in_progress" {
			inProgressCount++
		}
	}

	if inProgressCount > 1 {
		return "", errors.New("only one task can be in_progress at a time")
	}

	tm.items = items
	return tm.render(), nil
}

// render is the internal unlocked rendering method
func (tm *TodoManager) render() string {
	if len(tm.items) == 0 {
		return fmt.Sprintf("%s☐ No todos yet%s", todoPendingColor, reset)
	}

	var lines []string
	for _, todo := range tm.items {
		mark := "☐"
		if todo.Status == "completed" {
			mark = "☒"
		}

		var line string
		switch todo.Status {
		case "completed":
			line = fmt.Sprintf("%s%s%s %s%s", todoCompletedColor, strikethrough, mark, todo.Content, reset)
		case "in_progress":
			line = fmt.Sprintf("%s%s %s%s", todoProgressColor, mark, todo.Content, reset)
		default:
			line = fmt.Sprintf("%s%s %s%s", todoPendingColor, mark, todo.Content, reset)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Render returns the formatted todo list (thread-safe)
func (tm *TodoManager) Render() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.render()
}

// stats is the internal unlocked stats method
func (tm *TodoManager) stats() map[string]int {
	completed := 0
	inProgress := 0
	for _, todo := range tm.items {
		if todo.Status == "completed" {
			completed++
		} else if todo.Status == "in_progress" {
			inProgress++
		}
	}

	return map[string]int{
		"total":       len(tm.items),
		"completed":   completed,
		"in_progress": inProgress,
	}
}

// Stats returns todo statistics (thread-safe)
func (tm *TodoManager) Stats() map[string]int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.stats()
}

type spinner struct {
	label   string
	frames  []string
	stopCh  chan struct{}
	doneCh  chan struct{}
	mu      sync.Mutex
	running bool
}

func newSpinner(label string) *spinner {
	return &spinner{
		label:  label,
		frames: spinnerFrames,
	}
}

func (s *spinner) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.running = true
	go s.loop()
}

func (s *spinner) loop() {
	ticker := time.NewTicker(spinnerTick)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-s.stopCh:
			fmt.Printf("\r%*s\r", len(s.label)+2, "")
			close(s.doneCh)
			return
		case <-ticker.C:
			fmt.Printf("\r%s %s", s.label, s.frames[frame%len(s.frames)])
			frame++
		}
	}
}

func (s *spinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	close(s.stopCh)
	<-s.doneCh
	s.running = false
}

func main() {
	cfg := loadConfig()
	history := make([]Message, 0)

	// Initialize with initial reminder
	pendingContextBlocks = append(pendingContextBlocks, ContentBlock{
		Type: "text",
		Text: initialReminder,
	})

	fmt.Printf("Tiny CC Agent (Go) -- cwd: %s\n", cfg.WorkDir)
	fmt.Println("Type \"exit\" or \"quit\" to leave.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("User: ")
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if lower == "exit" || lower == "quit" || lower == "q" {
			break
		}

		// Inject reminders into user message
		content := injectReminders(line)
		history = append(history, Message{Role: "user", Content: content})

		updated, err := query(cfg, history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}
		history = updated
	}
}

func loadConfig() Config {
	workDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-4"
	}

	maxTokens := defaultMaxTokens
	if maxTokensStr := strings.TrimSpace(os.Getenv("OPENAI_MAX_TOKENS")); maxTokensStr != "" {
		if parsed, err := strconv.Atoi(maxTokensStr); err == nil && parsed > 0 {
			maxTokens = parsed
		}
	}

	cfg := Config{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		Model:     model,
		WorkDir:   workDir,
		MaxResult: maxTokens,
		Debug:     strings.ToLower(strings.TrimSpace(os.Getenv("DEBUG"))) == "true",
		Stream:    strings.ToLower(strings.TrimSpace(os.Getenv("OPENAI_STREAM"))) != "false",
	}

	if cfg.APIKey == "" {
		log.Fatal("OPENAI_API_KEY required")
	}

	return cfg
}

func query(cfg Config, messages []Message) ([]Message, error) {
	sysPrompt := fmt.Sprintf(systemPrompt, cfg.WorkDir)

	// 在消息前面添加 system message
	fullMessages := make([]Message, 0, len(messages)+1)
	fullMessages = append(fullMessages, Message{
		Role:    "system",
		Content: sysPrompt,
	})
	fullMessages = append(fullMessages, messages...)

	for idx := 0; idx < maxAgentIterations; idx++ {
		spin := newSpinner("Waiting for model")
		spin.Start()
		resp, err := callOpenAI(cfg, fullMessages)
		spin.Stop()
		if err != nil {
			return messages, err
		}

		if len(resp.Choices) == 0 {
			return messages, errors.New("no choices in response")
		}

		choice := resp.Choices[0]
		assistantMsg := choice.Message

		// 打印文本内容
		if assistantMsg.Content != "" {
			fmt.Println(assistantMsg.Content)
		}

		// 追加 assistant 消息到历史
		messages = append(messages, assistantMsg)
		fullMessages = append(fullMessages, assistantMsg)

		// 检查是否有 tool calls
		if choice.FinishReason == "tool_calls" && len(assistantMsg.ToolCalls) > 0 {
			// 执行所有工具
			for _, tc := range assistantMsg.ToolCalls {
				result := dispatchToolCall(cfg, tc)
				messages = append(messages, result)
				fullMessages = append(fullMessages, result)
			}
			continue
		}

		// Track rounds without todo usage
		agentState.mu.Lock()
		agentState.roundsWithoutTodo++
		if agentState.roundsWithoutTodo > 10 {
			ensureContextBlock(nagReminder)
		}
		agentState.mu.Unlock()

		return messages, nil
	}

	return messages, errors.New("agent max iterations reached")
}

func callOpenAI(cfg Config, messages []Message) (*APIResponse, error) {
	baseURL := cfg.BaseURL
	var endpoint string

	// Handle different URL formats
	if strings.HasSuffix(baseURL, "#") {
		// # suffix: use the URL as-is (remove #)
		endpoint = strings.TrimSuffix(baseURL, "#")
	} else if strings.HasSuffix(baseURL, "/v1") {
		// Base URL already ends with /v1: append /chat/completions
		endpoint = baseURL + "/chat/completions"
	} else if strings.HasSuffix(baseURL, "/") {
		// / suffix: append chat/completions directly (ignore v1)
		endpoint = baseURL + "chat/completions"
	} else {
		// Default: append /v1/chat/completions
		endpoint = baseURL + "/v1/chat/completions"
	}

	// Log request URL (only if DEBUG=true)
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "\n[DEBUG] Request URL: %s\n", endpoint)
	}

	body := map[string]interface{}{
		"model":      cfg.Model,
		"messages":   messages,
		"tools":      toolDefinitions(),
		"max_tokens": cfg.MaxResult,
		"stream":     cfg.Stream,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// Log request payload (only if DEBUG=true)
	if cfg.Debug {
		var prettyPayload bytes.Buffer
		if err := json.Indent(&prettyPayload, payload, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "[DEBUG] Request Payload:\n%s\n", prettyPayload.String())
		}
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	// OpenAI uses Bearer token
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// Log request headers (only if DEBUG=true)
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Request Headers:\n")
		for key, values := range req.Header {
			for _, value := range values {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", key, value)
			}
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Handle streaming response
	if cfg.Stream {
		return handleStreamingResponse(cfg, resp)
	}

	// Handle non-streaming response
	return handleNonStreamingResponse(cfg, resp)
}

func dispatchToolCall(cfg Config, tc ToolCall) Message {
	// 解析 arguments
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
		return Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    fmt.Sprintf("Error parsing arguments: %v", err),
		}
	}

	// Display tool call with appropriate formatting
	var displayText string
	switch tc.Function.Name {
	case "TodoWrite":
		displayText = "updating todos"
	default:
		displayText = fmt.Sprintf("%v", input)
	}
	prettyToolLine(tc.Function.Name, displayText)

	var result string
	var err error

	switch tc.Function.Name {
	case "bash":
		result, err = runBash(cfg, input)
	case "read_file":
		result, err = runRead(cfg, input)
	case "write_file":
		result, err = runWrite(cfg, input)
	case "edit_text":
		result, err = runEdit(cfg, input)
	case "TodoWrite":
		result, err = runTodoUpdate(cfg, input)
	default:
		err = fmt.Errorf("unknown tool: %s", tc.Function.Name)
	}

	if err != nil {
		result = err.Error()
	}

	prettySubLine(clampText(result, 2000))

	return Message{
		Role:       "tool",
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
		Content:    clampText(result, cfg.MaxResult),
	}
}

func runBash(cfg Config, input map[string]interface{}) (string, error) {
	command := strings.TrimSpace(getString(input, "command"))
	if command == "" {
		return "", errors.New("missing bash.command")
	}
	if isDangerousCommand(command) {
		return "", errors.New("blocked dangerous command")
	}
	timeout := getIntOrDefault(input, "timeout_ms", 30000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = cfg.WorkDir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "(timeout)", nil
	}
	output := strings.TrimSpace(strings.Join([]string{stdout.String(), stderr.String()}, "\n"))
	if output == "" {
		output = "(no output)"
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			output = fmt.Sprintf("%s\n(exit error: %v)", output, err)
			err = nil
		}
	}
	return clampText(output, maxToolResultChars), err
}

func runRead(cfg Config, input map[string]interface{}) (string, error) {
	path := getString(input, "path")
	abs, err := safePath(cfg.WorkDir, path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	text := string(data)
	lines := strings.Split(text, "\n")

	start := 0
	if val, ok := getOptionalInt(input, "start_line"); ok {
		if val < 1 {
			val = 1
		}
		start = val - 1
		if start > len(lines) {
			start = len(lines)
		}
	}
	end := len(lines)
	if val, ok := getOptionalInt(input, "end_line"); ok {
		if val >= 0 {
			if val < start {
				end = start
			} else if val > len(lines) {
				end = len(lines)
			} else {
				end = val
			}
		}
	}
	if start > end {
		start = end
	}
	sliced := strings.Join(lines[start:end], "\n")
	maxChars := getIntOrDefault(input, "max_chars", maxToolResultChars)
	return clampText(sliced, maxChars), nil
}

func runWrite(cfg Config, input map[string]interface{}) (string, error) {
	path := getString(input, "path")
	abs, err := safePath(cfg.WorkDir, path)
	if err != nil {
		return "", err
	}
	content := getString(input, "content")
	mode := strings.ToLower(getString(input, "mode"))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if mode == "append" {
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return "", err
		}
	} else {
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	bytesLen := len([]byte(content))
	rel, err := filepath.Rel(cfg.WorkDir, abs)
	if err != nil {
		rel = abs
	}
	return fmt.Sprintf("wrote %d bytes to %s", bytesLen, rel), nil
}

func runEdit(cfg Config, input map[string]interface{}) (string, error) {
	path := getString(input, "path")
	abs, err := safePath(cfg.WorkDir, path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	text := string(data)
	action := strings.ToLower(getString(input, "action"))
	switch action {
	case "replace":
		findStr := getString(input, "find")
		if findStr == "" {
			return "", errors.New("edit_text.replace missing find")
		}
		replaceStr := getString(input, "replace")
		updated := strings.ReplaceAll(text, findStr, replaceStr)
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("replace done (%d bytes)", len([]byte(updated))), nil
	case "insert":
		insertAfter := getIntOrDefault(input, "insert_after", -1)
		newText := getString(input, "new_text")
		lines := strings.Split(text, "\n")
		idx := insertAfter
		if idx < -1 {
			idx = -1
		}
		if idx >= len(lines) {
			idx = len(lines) - 1
		}
		result := make([]string, 0, len(lines)+1)
		if idx >= 0 {
			result = append(result, lines[:idx+1]...)
			result = append(result, newText)
			result = append(result, lines[idx+1:]...)
		} else {
			result = append(result, newText)
			result = append(result, lines...)
		}
		updated := strings.Join(result, "\n")
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("inserted after line %d", insertAfter), nil
	case "delete_range":
		rngRaw, ok := input["range"].([]interface{})
		if !ok || len(rngRaw) != 2 {
			return "", errors.New("edit_text.delete_range invalid range")
		}
		start := toInt(rngRaw[0])
		end := toInt(rngRaw[1])
		if start < 0 || end < start {
			return "", errors.New("edit_text.delete_range invalid range")
		}
		lines := strings.Split(text, "\n")
		if start > len(lines) {
			start = len(lines)
		}
		if end > len(lines) {
			end = len(lines)
		}
		updated := strings.Join(append(append([]string{}, lines[:start]...), lines[end:]...), "\n")
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("deleted lines [%d, %d)", start, end), nil
	default:
		return "", fmt.Errorf("unsupported edit_text.action: %s", action)
	}
}

func runTodoUpdate(cfg Config, input map[string]interface{}) (string, error) {
	itemsRaw, ok := input["items"]
	if !ok {
		return "", errors.New("missing items parameter")
	}

	itemsList, ok := itemsRaw.([]interface{})
	if !ok {
		return "", errors.New("items must be an array")
	}

	items := make([]TodoItem, 0, len(itemsList))
	for i, rawItem := range itemsList {
		itemMap, ok := rawItem.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("item %d is not an object", i)
		}

		id := getString(itemMap, "id")
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}

		content := getString(itemMap, "content")
		activeForm := getString(itemMap, "activeForm")
		status := getString(itemMap, "status")
		if status == "" {
			status = "pending"
		}

		items = append(items, TodoItem{
			ID:         id,
			Content:    content,
			Status:     status,
			ActiveForm: activeForm,
		})
	}

	boardView, err := todoBoard.Update(items)
	if err != nil {
		return "", err
	}

	// Reset rounds counter
	agentState.mu.Lock()
	agentState.roundsWithoutTodo = 0
	agentState.mu.Unlock()

	stats := todoBoard.Stats()
	var summary string
	if stats["total"] == 0 {
		summary = "No todos have been created."
	} else {
		summary = fmt.Sprintf("Status updated: %d completed, %d in progress.",
			stats["completed"], stats["in_progress"])
	}

	if summary != "" {
		return boardView + "\n\n" + summary, nil
	}
	return boardView, nil
}

func safePath(workDir, p string) (string, error) {
	candidate := strings.TrimSpace(p)
	if candidate == "" {
		return "", errors.New("path required")
	}
	var joined string
	if filepath.IsAbs(candidate) {
		joined = filepath.Clean(candidate)
	} else {
		joined = filepath.Join(workDir, candidate)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	workAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, workAbs+string(os.PathSeparator)) && abs != workAbs {
		return "", errors.New("path escapes workspace")
	}
	return abs, nil
}

func isDangerousCommand(cmd string) bool {
	lowered := strings.ToLower(cmd)
	danger := []string{
		"rm -rf /",
		"shutdown",
		"reboot",
		"sudo ",
		"halt",
	}
	for _, token := range danger {
		if strings.Contains(lowered, token) {
			return true
		}
	}
	return false
}

func clampText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len([]rune(s)) <= limit {
		return s
	}
	runes := []rune(s)
	truncated := string(runes[:limit])
	extras := len(runes) - limit
	return fmt.Sprintf("%s\n\n...<truncated %d chars>", truncated, extras)
}

func clampForLog(s string) string {
	return clampText(s, 2000)
}

func injectReminders(userText string) interface{} {
	if len(pendingContextBlocks) == 0 {
		return userText // Simple string
	}
	blocks := make([]ContentBlock, len(pendingContextBlocks))
	copy(blocks, pendingContextBlocks)
	blocks = append(blocks, ContentBlock{Type: "text", Text: userText})
	pendingContextBlocks = nil
	return blocks
}

func ensureContextBlock(text string) {
	for _, block := range pendingContextBlocks {
		if block.Text == text {
			return
		}
	}
	pendingContextBlocks = append(pendingContextBlocks, ContentBlock{
		Type: "text",
		Text: text,
	})
}

func getString(input map[string]interface{}, key string) string {
	if input == nil {
		return ""
	}
	if val, ok := input[key]; ok {
		switch v := val.(type) {
		case string:
			return v
		case json.Number:
			return v.String()
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			return strconv.Itoa(v)
		}
	}
	return ""
}

func getIntOrDefault(input map[string]interface{}, key string, def int) int {
	if val, ok := getOptionalInt(input, key); ok {
		return val
	}
	return def
}

func getOptionalInt(input map[string]interface{}, key string) (int, bool) {
	if input == nil {
		return 0, false
	}
	val, ok := input[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case json.Number:
		num, err := v.Int64()
		if err == nil {
			return int(num), true
		}
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case json.Number:
		num, err := val.Int64()
		if err == nil {
			return int(num)
		}
	case float64:
		return int(val)
	case int:
		return val
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(val))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func prettyToolLine(kind, title string) {
	if title == "" {
		fmt.Printf("[tool] %s\n", kind)
		return
	}
	fmt.Printf("[tool] %s(%s)\n", kind, title)
}

func prettySubLine(text string) {
	fmt.Printf("  -> %s\n", text)
}

const systemPrompt = "You are a coding agent operating INSIDE the user's repository at %s.\n" +
	"Follow this loop strictly: plan briefly → use TOOLS to act directly on files/shell → report concise results.\n" +
	"Rules:\n" +
	"- Prefer taking actions with tools (read/write/edit/bash) over long prose.\n" +
	"- Keep outputs terse. Use bullet lists / checklists when summarizing.\n" +
	"- Never invent file paths. Ask via reads or list directories first if unsure.\n" +
	"- For edits, apply the smallest change that satisfies the request.\n" +
	"- For bash, avoid destructive or privileged commands; stay inside the workspace.\n" +
	"- Use the TodoWrite tool to maintain multi-step plans when needed.\n" +
	"- After finishing, summarize what changed and how to run or test."

func toolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "bash",
				"description": "Execute a shell command inside the project workspace. Use for scaffolding, formatting, running scripts, etc.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command":    map[string]interface{}{"type": "string", "description": "Shell command to run"},
						"timeout_ms": map[string]interface{}{"type": "integer", "minimum": 1000, "maximum": 120000},
					},
					"required":             []string{"command"},
					"additionalProperties": false,
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "read_file",
				"description": "Read a UTF-8 text file. Optionally slice by line range or clamp length.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":       map[string]interface{}{"type": "string"},
						"start_line": map[string]interface{}{"type": "integer", "minimum": 1},
						"end_line":   map[string]interface{}{"type": "integer", "minimum": -1},
						"max_chars":  map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 200000},
					},
					"required":             []string{"path"},
					"additionalProperties": false,
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "write_file",
				"description": "Create or overwrite/append a UTF-8 text file. Use overwrite unless explicitly asked to append.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":    map[string]interface{}{"type": "string"},
						"content": map[string]interface{}{"type": "string"},
						"mode":    map[string]interface{}{"type": "string", "enum": []string{"overwrite", "append"}, "default": "overwrite"},
					},
					"required":             []string{"path", "content"},
					"additionalProperties": false,
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "edit_text",
				"description": "Small, precise text edits. Choose one action: replace | insert | delete_range.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":         map[string]interface{}{"type": "string"},
						"action":       map[string]interface{}{"type": "string", "enum": []string{"replace", "insert", "delete_range"}},
						"find":         map[string]interface{}{"type": "string"},
						"replace":      map[string]interface{}{"type": "string"},
						"insert_after": map[string]interface{}{"type": "integer", "minimum": -1},
						"new_text":     map[string]interface{}{"type": "string"},
						"range":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer"}, "minItems": 2, "maxItems": 2},
					},
					"required":             []string{"path", "action"},
					"additionalProperties": false,
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "TodoWrite",
				"description": "Update the shared todo list (pending | in_progress | completed).",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"items": map[string]interface{}{
							"type": "array",
							"items": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"id":         map[string]interface{}{"type": "string"},
									"content":    map[string]interface{}{"type": "string"},
									"activeForm": map[string]interface{}{"type": "string"},
									"status":     map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
								},
								"required":             []string{"content", "activeForm", "status"},
								"additionalProperties": false,
							},
							"maxItems": maxTodoItems,
						},
					},
					"required":             []string{"items"},
					"additionalProperties": false,
				},
			},
		},
	}
}

// handleNonStreamingResponse processes standard JSON responses
func handleNonStreamingResponse(cfg Config, resp *http.Response) (*APIResponse, error) {
	// Read response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Log response (only if DEBUG=true)
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Response Status: %d %s\n", resp.StatusCode, resp.Status)
		fmt.Fprintf(os.Stderr, "[DEBUG] Response Headers:\n")
		for key, values := range resp.Header {
			for _, value := range values {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", key, value)
			}
		}
		var prettyResp bytes.Buffer
		if err := json.Indent(&prettyResp, data, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "[DEBUG] Response Body:\n%s\n\n", prettyResp.String())
		} else {
			fmt.Fprintf(os.Stderr, "[DEBUG] Response Body (raw):\n%s\n\n", clampForLog(string(data)))
		}
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("api error: status %d body %s", resp.StatusCode, clampForLog(string(data)))
	}

	var apiResp APIResponse
	if err := json.Unmarshal(data, &apiResp); err != nil {
		return nil, err
	}
	return &apiResp, nil
}

// handleStreamingResponse processes Server-Sent Events (SSE) stream responses
func handleStreamingResponse(cfg Config, resp *http.Response) (*APIResponse, error) {
	// Log response headers (only if DEBUG=true)
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Response Status: %d %s\n", resp.StatusCode, resp.Status)
		fmt.Fprintf(os.Stderr, "[DEBUG] Response Headers:\n")
		for key, values := range resp.Header {
			for _, value := range values {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", key, value)
			}
		}
		fmt.Fprintf(os.Stderr, "[DEBUG] Processing streaming response...\n")
	}

	if resp.StatusCode >= 400 {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("api error: status %d body %s", resp.StatusCode, clampForLog(string(data)))
	}

	// Process streaming response
	var finalContent strings.Builder
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if cfg.Debug {
			fmt.Fprintf(os.Stderr, "[DEBUG] SSE Line: %s\n", line)
		}

		// Skip empty lines and SSE event markers
		if strings.TrimSpace(line) == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		// Extract JSON data
		dataStr := strings.TrimPrefix(line, "data: ")
		if dataStr == "[DONE]" {
			break
		}

		// Parse the JSON chunk
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			if cfg.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Error parsing SSE chunk: %v\n", err)
			}
			continue
		}

		// Accumulate content
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			finalContent.WriteString(chunk.Choices[0].Delta.Content)
			fmt.Print(chunk.Choices[0].Delta.Content)
		}

		// Check for finish reason
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading stream: %v", err)
	}

	// Create a mock API response with the accumulated content
	return &APIResponse{
		Choices: []Choice{
			{
				Message: Message{
					Role:    "assistant",
					Content: finalContent.String(),
				},
				FinishReason: "stop",
			},
		},
	}, nil
}
