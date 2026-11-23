package loop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type Budget struct {
	MaxIterations int
	MaxTokens     int
	Timeout       time.Duration
}

func (b Budget) WithDefaults() Budget {
	if b.MaxIterations <= 0 {
		b.MaxIterations = 25
	}
	if b.MaxTokens <= 0 {
		b.MaxTokens = 200000
	}
	if b.Timeout <= 0 {
		b.Timeout = time.Hour
	}
	return b
}

type Loop struct {
	StopOnApproval bool
	provider.Provider
	Tools     *tool.Registry
	System    string
	Context   func(context.Context) string
	Model     string
	MaxTokens int
	Budget
}

func (l *Loop) Turn(ctx context.Context, history []provider.Message, user string) (string, []provider.Message, error) {
	if err := ctx.Err(); err != nil {
		return "", history, err
	}
	if l.Provider == nil {
		return "", history, fmt.Errorf("conversation provider is missing")
	}
	budget := l.Budget.WithDefaults()
	turnCtx, cancel := context.WithTimeout(ctx, budget.Timeout)
	defer cancel()
	history = append(slices.Clone(history), provider.Message{
		Role: "user", Content: []provider.Block{{Type: "text", Text: user}},
	})
	maxTokens := l.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	specs := l.Tools.Specs()
	remaining := budget.MaxTokens
	for i := 0; i < budget.MaxIterations; i++ {
		if turnCtx.Err() != nil {
			return stoppedByContext(ctx, history, budget.Timeout)
		}
		req := provider.Request{
			Model: l.Model, System: l.System, Messages: history, Tools: specs, MaxTokens: maxTokens,
		}
		if l.Context != nil {
			if extra := l.Context(turnCtx); extra != "" {
				if req.System != "" {
					req.System += "\n\n"
				}
				req.System += extra
			}
		}
		res, err := l.Complete(turnCtx, req)
		if turnCtx.Err() != nil {
			return stoppedByContext(ctx, history, budget.Timeout)
		}
		if err != nil {
			return "", history, fmt.Errorf("complete conversation: %w", err)
		}
		assistant := provider.Message{Role: "assistant", Content: res.Content}
		history = append(history, assistant)
		if res.Usage == (provider.Usage{}) {
			remaining -= min(remaining, provider.EstimateTokens(req, res))
		} else {
			remaining -= min(remaining, max(0, res.Usage.InputTokens))
			remaining -= min(remaining, max(0, res.Usage.OutputTokens))
		}

		var stop string
		if remaining == 0 {
			stop = fmt.Sprintf("Stopped: the token budget (%d) was reached.", budget.MaxTokens)
		}
		var results []provider.Block
		for _, block := range res.Content {
			if block.Type == "tool_use" {
				result := provider.Block{Type: "tool_result", ToolUseID: block.ID}
				if stop != "" {
					result.Text, result.IsError = stop, true
				} else {
					text, err := l.Tools.Run(turnCtx, block.Name, block.Input)
					if err != nil {
						result.Text, result.IsError = err.Error(), true
						if l.StopOnApproval && errors.Is(err, tool.ErrNoApprover) {
							stop = fmt.Sprintf("This job needs the user to do this step: %s.", block.Name)
						}
					} else {
						result.Text = text
					}
				}
				results = append(results, result)
			}
		}
		if len(results) > 0 {
			history = append(history, provider.Message{Role: "user", Content: results})
		}
		if turnCtx.Err() != nil {
			return stoppedByContext(ctx, history, budget.Timeout)
		}
		if stop != "" {
			return budgetStop(stop, history)
		}
		if len(results) == 0 {
			return assistant.Text(), history, nil
		}
	}
	return budgetStop(fmt.Sprintf("Stopped: the iteration budget (%d) was reached.", budget.MaxIterations), history)
}

func stoppedByContext(ctx context.Context, history []provider.Message, timeout time.Duration) (string, []provider.Message, error) {
	if err := ctx.Err(); err != nil {
		return "", history, err
	}
	return budgetStop(fmt.Sprintf("Stopped: the timeout budget (%s) was reached.", timeout), history)
}

func budgetStop(stop string, history []provider.Message) (string, []provider.Message, error) {
	last := history[len(history)-1]
	// Saved history must alternate, whether the turn stopped after tool results or before any answer.
	if last.Role == "user" {
		history = append(history, provider.Message{Role: "assistant", Content: []provider.Block{{Type: "text", Text: stop}}})
	}
	return stop, history, nil
}
