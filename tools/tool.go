package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools/builtin"
)

// Tool interface - Tool is any tool function the model can call (built-in tools for now, an MCP tool later)
// Note that when getting a tools list from an MCP server, the tools will be instantiated as many Tool values, one per listed tool.
type Tool interface {
	GetToolDefinition() model.ToolDef
	CallTool(ctx context.Context, args json.RawMessage) (string, error)
}

// setup built-in tools
func SetupBuiltinTools(toolList []string, workDir string) ([]Tool, error) {
	tools := []Tool{}
	// add the built-in tools specified in the config
	for _, t := range toolList {
		switch t {
		case "ReadFile":
			readFile, err := builtin.NewReadFileTool(workDir)
			if err != nil {
				return nil, err
			}
			tools = append(tools, readFile)
		// add more built-in tools here as needed
		default:
			return nil, fmt.Errorf("unsupported built-in tool: %s", t)
		}
	}
	return tools, nil
}

// creates a tool registry
func SetupToolRegistry(builtinTools []Tool, mcpTools []Tool) (*ToolRegistry, error) {
	tools := []Tool{}
	// first add the built-in tools (ReadFile Tool to start)
	tools = append(tools, builtinTools...)

	// add the MCP tools specified in the config file
	tools = append(tools, mcpTools...)

	// create a registry from the tools list
	reg, err := NewToolRegistry(tools)
	if err != nil {
		return nil, err
	}
	return reg, nil
}

// tool registry - keeps track of all registered tools that the model can call
type ToolRegistry struct {
	mu       sync.RWMutex         // allows concurrent reads with RLock() - good for frequent-read, rare-write structs (like a tool registry)
	tools    map[string]toolEntry // maps tool names to their corresponding tool entries for O(1) access
	tooldefs []model.ToolDef      // ordered list of registered tool definitions - this is what is sent to the LLM as context - this is better for caching purposes on the model end (order is same each time)
}

// an entry in the tool registry
type toolEntry struct {
	tool Tool
	def  model.ToolDef // captured once, at Register time
}

func NewToolRegistry(tools []Tool) (*ToolRegistry, error) {
	// initialize an empty tool registry
	reg := &ToolRegistry{
		tools:    make(map[string]toolEntry),
		tooldefs: []model.ToolDef{},
	}
	// then register each tool
	for _, tool := range tools {
		if err := reg.Register(tool); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// isNilTool reports whether t is nil, including the "typed nil" case.
// [agent] An interface value is a (type, value) pair. `var p *MyTool; var t Tool = p` gives t the
// type *MyTool and the value nil, so `t == nil` is false even though calling a method that
// dereferences the receiver would panic. reflect is the only way to look inside the pair.
// Trade-off: reflect is slower and less obvious than a plain nil check, but this runs only at
// registration (rare), never per request. Downside: a tool that deliberately supports a nil
// receiver would be rejected too; nothing here does.
// func isNilTool(t Tool) bool {
// 	if t == nil {
// 		return true
// 	}
// 	v := reflect.ValueOf(t)
// 	switch v.Kind() {
// 	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
// 		return v.IsNil()
// 	}
// 	return false
// }

// Adds a new tool to the tool registry. It returns an error if the tool name is empty or if a tool with the same name is already registered.
func (r *ToolRegistry) Register(t Tool) error {
	// if passed Tool is nil, error out
	// [agent] This catches only a nil interface; a typed nil pointer (e.g. (*MyTool)(nil)) passes and would panic below. Deliberately not handled: nothing here creates one.
	if t == nil {
		return fmt.Errorf("cannot register a nil tool")
	}
	toolDef := t.GetToolDefinition()
	toolName := toolDef.Function.Name
	if toolName == "" {
		return fmt.Errorf("tool name cannot be empty")
	}
	// tools that match an existing tool name do NOT overwrite - Lock() is called on writes
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[toolName]; exists {
		return fmt.Errorf("tool with name %q already registered", toolName)
	}
	r.tools[toolName] = toolEntry{
		tool: t,
		def:  toolDef,
	}
	r.rebuildToolDefList()
	return nil
}

// assuming that tool map exists, builds / rebuilds the ordered (sorted) list of tool definitions for the LLM context
// since this modifies the registry, ANY method that calls this MUST already hold the write lock (Lock) on the registry.
func (r *ToolRegistry) rebuildToolDefList() {
	r.tooldefs = make([]model.ToolDef, 0, len(r.tools))
	for _, toolEntry := range r.tools {
		r.tooldefs = append(r.tooldefs, toolEntry.def)
	}
	// sort the tool definitions by name to ensure consistent order
	sort.Slice(r.tooldefs, func(i, j int) bool {
		return r.tooldefs[i].Function.Name < r.tooldefs[j].Function.Name
	})
}

// Deletes a tool from the registry by name. It returns true if the tool was found and removed, false otherwise.
func (r *ToolRegistry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	r.rebuildToolDefList()
	return true
}

// Looks up a tool by name. It returns the tool and true if found, or nil and false otherwise.
func (r *ToolRegistry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t.tool, ok
}

// Returns the cached tool definitions, sorted by name.
// Callers must not modify the returned slice.
func (r *ToolRegistry) GetToolDefs() []model.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tooldefs
}
