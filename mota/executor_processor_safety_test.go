package mota

import (
	"context"
	"strings"
	"testing"
	"time"
)

type executorPanickingProcessor struct{}

func (executorPanickingProcessor) Process(
	context.Context,
	*TaskContext,
) (*ProcessedOutput, error) {
	panic("sensitive processor panic")
}

type executorNilOutputProcessor struct{}

func (executorNilOutputProcessor) Process(
	context.Context,
	*TaskContext,
) (*ProcessedOutput, error) {
	return nil, nil
}

func TestExecuteProcessorRecoversPanic(t *testing.T) {
	executor := newTestExecutor(t, executorTestConfig(t), executorPanickingProcessor{})
	_, err := executor.Execute(
		context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindRust, ExecutionModeLocal),
		&TaskContext{Params: map[string]any{}},
		map[string]any{},
		time.Second,
	)
	execution := wantExecutionCode(t, err, "processor_failed")
	if strings.Contains(execution.Detail, "sensitive") {
		t.Fatalf("panic value leaked into execution detail: %q", execution.Detail)
	}
}

func TestExecuteProcessorRejectsNilOutput(t *testing.T) {
	executor := newTestExecutor(t, executorTestConfig(t), executorNilOutputProcessor{})
	_, err := executor.Execute(
		context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindRust, ExecutionModeLocal),
		&TaskContext{Params: map[string]any{}},
		map[string]any{},
		time.Second,
	)
	wantExecutionCode(t, err, "invalid_executor_output")
}
