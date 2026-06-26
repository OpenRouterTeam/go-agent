package agent

import "context"

func StepCountIs(limit int) StopCondition {
	return func(_ context.Context, steps []StepResult) (bool, error) { return len(steps) >= limit, nil }
}
func HasToolCall(name string) StopCondition {
	return func(_ context.Context, steps []StepResult) (bool, error) {
		for _, s := range steps {
			for _, c := range s.ToolCalls {
				if c.Name == name {
					return true, nil
				}
			}
		}
		return false, nil
	}
}
func MaxTokensUsed(limit int64) StopCondition {
	return func(_ context.Context, steps []StepResult) (bool, error) {
		var total int64
		for _, s := range steps {
			if s.Usage != nil {
				total += s.Usage.TotalTokens
			}
		}
		return total >= limit, nil
	}
}
func MaxCost(limit float64) StopCondition {
	return func(_ context.Context, steps []StepResult) (bool, error) {
		var total float64
		for _, s := range steps {
			if s.Usage != nil {
				if v, ok := s.Usage.Cost.GetOrZero(); ok {
					total += v
				}
			}
		}
		return total >= limit, nil
	}
}
func FinishReasonIs(reason string) StopCondition {
	return func(_ context.Context, steps []StepResult) (bool, error) {
		for _, s := range steps {
			if s.FinishReason == reason {
				return true, nil
			}
		}
		return false, nil
	}
}
func IsStopConditionMet(ctx context.Context, conditions []StopCondition, steps []StepResult) (bool, error) {
	for _, c := range conditions {
		if c == nil {
			continue
		}
		ok, err := c(ctx, steps)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
