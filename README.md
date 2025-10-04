# Mini Claude Code Agent (Go)

A minimal AI coding agent in Go that provides file operations and shell execution through OpenAI-compatible chat APIs.

## Features

- 🤖 **Agentic Loop**: Automatically executes multiple tool calls to complete complex tasks
- 📁 **File Operations**: Read, write, and edit files with safety constraints
- 🔧 **Shell Execution**: Run bash commands with timeout and dangerous command blocking
- 🔒 **Path Sandbox**: All file operations are restricted to the workspace directory
- 🎯 **OpenAI Compatible**: Works with OpenAI API and compatible services (Moonshot, etc.)
- 📊 **Debug Mode**: Optional detailed logging of API requests/responses
- ⚡ **Single Binary**: No dependencies, easy deployment

## Quick Start

### Prerequisites

- Go 1.21 or higher
- OpenAI API key (or compatible service API key)

### Build

```bash
cd mini-claude-code-go
go build agent.go
```

This creates a single executable binary `agent` (~8MB).

### Run

```bash
# Set your API key
export OPENAI_API_KEY="sk-..."

# Run the agent
./agent
```

## Configuration

The agent is configured entirely through environment variables:

### Required

| Variable | Description |
|----------|-------------|
| `OPENAI_API_KEY` | Your OpenAI API key (or use `ANTHROPIC_API_KEY`) |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENAI_BASE_URL` | `https://api.openai.com` | API endpoint (or use `ANTHROPIC_BASE_URL`) |
| `OPENAI_MODEL` | `gpt-4` | Model to use (or use `ANTHROPIC_MODEL`) |
| `DEBUG` | `false` | Enable debug logging (`true` or `false`) |

### Examples

**Using OpenAI:**
```bash
export OPENAI_API_KEY="sk-..."
export OPENAI_MODEL="gpt-4-turbo"
./agent
```

**Using Moonshot (OpenAI-compatible):**
```bash
export OPENAI_API_KEY="sk-..."
export OPENAI_BASE_URL="https://api.moonshot.cn/v1"
export OPENAI_MODEL="moonshot-v1-8k"
./agent
```

**Enable debug logging:**
```bash
DEBUG=true ./agent
```

## Usage

Once started, you'll see a REPL prompt:

```
Tiny CC Agent (Go) -- cwd: /path/to/workspace
Type "exit" or "quit" to leave.

User:
```

Type natural language commands and the agent will use tools to accomplish tasks.

### Example Session

```
User: create a hello.txt file with "Hello World" content
[tool] write_file(map[content:Hello World path:hello.txt])
  -> wrote 11 bytes to hello.txt
Done! Created hello.txt with the requested content.

User: read the file
[tool] read_file(map[path:hello.txt])
  -> Hello World
The file contains: "Hello World"

User: append a new line "Goodbye"
[tool] read_file(map[path:hello.txt])
  -> Hello World
[tool] write_file(map[content:Hello World
Goodbye mode:append path:hello.txt])
  -> wrote 19 bytes to hello.txt
Done! Appended "Goodbye" to hello.txt

User: exit
```

### Exit Commands

Type any of these to exit:
- `exit`
- `quit`
- `q`
- Press `Ctrl+D` (EOF)

## Available Tools

The agent has access to 4 tools:

### 1. bash

Execute shell commands in the workspace directory.

**Features:**
- Default 30s timeout (configurable up to 120s via `timeout_ms`)
- Blocks dangerous commands: `rm -rf /`, `shutdown`, `reboot`, `sudo`, `halt`
- Captures both stdout and stderr

**Example:**
```
User: run ls -la
```

### 2. read_file

Read UTF-8 text files with optional line range and character limit.

**Parameters:**
- `path` (required): File path (relative to workspace)
- `start_line` (optional): Starting line number (1-based)
- `end_line` (optional): Ending line number (-1 for end of file)
- `max_chars` (optional): Maximum characters to return

**Example:**
```
User: read the first 10 lines of README.md
```

### 3. write_file

Create or modify files with overwrite or append mode.

**Parameters:**
- `path` (required): File path (relative to workspace)
- `content` (required): Content to write
- `mode` (optional): `overwrite` (default) or `append`

**Features:**
- Automatically creates parent directories
- Returns bytes written and relative path

**Example:**
```
User: create a config.json file with default settings
```

### 4. edit_text

Make precise edits to existing files.

**Actions:**
- `replace`: Find and replace text
  - Parameters: `find`, `replace`
- `insert`: Insert text after a specific line
  - Parameters: `insert_after` (line number, -1 for beginning), `new_text`
- `delete_range`: Delete a range of lines
  - Parameters: `range` [start, end) (exclusive end)

**Example:**
```
User: replace "old_function" with "new_function" in main.go
```

## Security

### Path Sandbox

All file operations are restricted to the workspace directory:

- Absolute paths are checked to ensure they're within the workspace
- Relative paths are resolved against the workspace
- Symbolic links are resolved and validated
- Path traversal attempts (e.g., `../../../etc/passwd`) are blocked

### Command Blocking

The following dangerous command patterns are blocked:
- `rm -rf /`
- `shutdown`
- `reboot`
- `sudo `
- `halt`

### Output Limits

Tool outputs are clamped to 100,000 characters to prevent memory issues.

## Development

### Project Structure

```
mini-claude-code-go/
├── agent.go                 # Single-file implementation (~800 lines)
├── go.mod                   # Go module definition
├── go.sum                   # Dependency checksums
└── README.md                # This file
```

### Code Organization

The code follows a clear structure:
1. Imports and constants
2. Type definitions (Config, Message, ToolCall, etc.)
3. Spinner (UX component)
4. Main function (REPL loop)
5. Config loading
6. Query function (agentic loop)
7. API client (callOpenAI)
8. Tool dispatcher
9. Tool implementations (bash, read, write, edit)
10. Safety layer (safePath, isDangerousCommand)
11. Utility functions
12. System prompt and tool definitions

### Testing

Build and test:
```bash
# Build
go build agent.go

# Run tests (if any)
go test ./...

# Vet code
go vet agent.go

# Format code
go fmt agent.go
```

### Debug Mode

Enable detailed API logging:

```bash
DEBUG=true ./agent
```

This will log to stderr:
- Request URL
- Request payload (pretty-printed JSON)
- Response status
- Response body (pretty-printed JSON)

Normal output to stdout is unaffected.

## Architecture

### Agentic Loop

The agent operates in a loop (max 20 iterations):

```
1. Send messages + tool definitions to API
2. Receive response
   - If text content: print it
   - If tool calls: execute all tools
3. Append results to conversation history
4. If finish_reason == "tool_calls": goto 1
5. Otherwise: done
```

### API Format

Uses OpenAI Chat Completions format:
- Endpoint: `/v1/chat/completions`
- Authentication: `Authorization: Bearer {key}`
- System prompt as first message with `role: "system"`
- Tool results as messages with `role: "tool"`

### Message Flow

```
User Input
  ↓
[{role: "system", content: "You are a coding agent..."},
 {role: "user", content: "create a file"}]
  ↓
API Call
  ↓
{role: "assistant", tool_calls: [{function: {name: "write_file", ...}}]}
  ↓
Execute Tools
  ↓
{role: "tool", tool_call_id: "...", content: "wrote 10 bytes"}
  ↓
API Call (continues until finish_reason != "tool_calls")
```

## Comparison with Python Version

This Go implementation is functionally equivalent to the [Python version](https://github.com/shareAI-lab/mini_claude_code/blob/main/v1_basic_agent.py):

| Feature | Python | Go |
|---------|--------|-----|
| Single file | ✅ | ✅ |
| File operations | ✅ | ✅ |
| Shell execution | ✅ | ✅ |
| Path sandbox | ✅ | ✅ |
| Dangerous command blocking | ✅ | ✅ |
| Agentic loop | ✅ | ✅ |
| Spinner | ✅ | ✅ |
| **API format** | Anthropic | **OpenAI** |
| **Binary size** | N/A (requires Python) | **8MB** |
| **Startup time** | ~1-2s | **<100ms** |
| **Memory usage** | ~50-80MB | **~20-30MB** |
| **Deployment** | Requires Python env | **Single binary** |

### Key Differences

1. **API Format**: Go version uses OpenAI format (`/v1/chat/completions`) instead of Anthropic format (`/v1/messages`)
2. **Performance**: Go version has faster startup and lower memory usage
3. **Deployment**: Go version compiles to a single binary with no runtime dependencies
4. **Compatibility**: Go version works with any OpenAI-compatible API

## Troubleshooting

### "OPENAI_API_KEY or ANTHROPIC_API_KEY required"

Set one of these environment variables:
```bash
export OPENAI_API_KEY="sk-..."
```

### "api error: status 404"

Check your `OPENAI_BASE_URL`. Common values:
- OpenAI: `https://api.openai.com` (default)
- Moonshot: `https://api.moonshot.cn/v1`

The URL should NOT include `/chat/completions` (it's added automatically).

### "path escapes workspace"

All file operations must be within the current working directory. Absolute paths or `..` that escape the workspace are blocked for security.

### "blocked dangerous command"

Commands containing dangerous patterns are blocked. Use safer alternatives or break down the task.

## License

MIT License - see the original Python version for details.

## Credits

- Original Python version: [shareAI-lab/mini_claude_code](https://github.com/shareAI-lab/mini_claude_code)
- Go port: Adapted to use OpenAI-compatible APIs

## Contributing

This is a minimal reference implementation. For enhancements:
1. Fork the repository
2. Make your changes
3. Test thoroughly
4. Submit a pull request

Keep it simple - the goal is to stay under 1000 lines in a single file.
