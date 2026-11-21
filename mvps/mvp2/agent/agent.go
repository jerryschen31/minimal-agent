package agent

import "context"

type Provider interface {
	Chat(ctx context.Context, prompt string) (string, error)
}
