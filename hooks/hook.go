package hooks

import (
	"context"

	"github.com/jerryschen31/minimal-agent/model"
)

// Hook is an interface for implementing tool call checks before they are executed; an error means the tool call was denied.
type Hook interface {
	BeforeTool(ctx context.Context, tc model.ToolCall) error
}
