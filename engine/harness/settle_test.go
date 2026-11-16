package harness

import (
	"context"
	"iter"
	"testing"

	"antifox/engine/llm"
	"antifox/engine/tool"
)

// TestInitialSettlementForcesWrapUp guards the forced-wrap-up path (Session.Settle):
// the loop must start in settlement mode (finishing with InitialSettlement rather
// than "completed") and hide DisabledTools from the very first request, while
// leaving the write-back tools visible.
func TestInitialSettlementForcesWrapUp(t *testing.T) {
	var calls []llm.CompletionRequest
	deps := QueryDeps{
		CallModel: func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			calls = append(calls, req)
			return func(yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "已写回 3 条事实"}, nil) {
					return
				}
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			}
		},
	}
	tools := tool.NewRegistry(
		tool.Build(tool.Spec{Name: "Bash", Description: "probe", Schema: map[string]any{}}),
		tool.Build(tool.Spec{Name: "record_fact", Description: "write back", Schema: map[string]any{}}),
	)
	in := QueryInput{
		System:            []string{"sys"},
		Messages:          []llm.Message{llm.UserText("预算耗尽，请写回并总结")},
		Tools:             tools,
		Settlement:        &Settlement{MaxTurns: 2},
		InitialSettlement: ReasonTimeout,
		DisabledTools:     []string{"Bash"},
	}
	var terminal *Terminal
	for ev, err := range Query(context.Background(), in, deps) {
		if err != nil {
			t.Fatalf("Query error: %v", err)
		}
		if ev.Kind == KindResult {
			terminal = ev.Terminal
		}
	}
	if terminal == nil {
		t.Fatal("no KindResult terminal event")
	}
	if terminal.Reason != ReasonTimeout {
		t.Fatalf("terminal reason = %q, want %q (forced wrap-up must not finish as completed)", terminal.Reason, ReasonTimeout)
	}
	if terminal.Text != "已写回 3 条事实" {
		t.Fatalf("terminal text = %q, want the wrap-up summary", terminal.Text)
	}
	if len(calls) != 1 {
		t.Fatalf("model calls = %d, want 1 (text-only wrap-up should end immediately)", len(calls))
	}
	var sawWriteBack bool
	for _, s := range calls[0].Tools {
		if s.Name == "Bash" {
			t.Fatal("Bash schema must be hidden during forced wrap-up")
		}
		if s.Name == "record_fact" {
			sawWriteBack = true
		}
	}
	if !sawWriteBack {
		t.Fatal("write-back tool schema must stay visible during forced wrap-up")
	}
}
