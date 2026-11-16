package agent

import (
	"context"
	"errors"
	"iter"
	"strings"

	"antifox/db"
	"antifox/engine/agentcore"
	"antifox/engine/harness"
	"antifox/engine/llm"
)

// captureRun drives one agent turn-to-completion over Session.Prompt and emits a
// coalesced ActivityRecord per execution step (tool_use / tool_result / text /
// thinking / result). It is shared by every LLM agent in the system (worker,
// planner, …) so their execution is visible instead of a black box — the old
// agentcore.Run discarded every event. The emitted records carry only
// Kind/Tool/ToolUseID/IsError/Summary/Detail; the caller's emit fills in
// IntentID/Worker. Returns the final assistant text + terminal error.
//
// KindText/KindThinking arrive as streaming deltas (one event per fragment); a
// contiguous run is coalesced into a single record so the trace shows whole
// messages, not dozens of fragments.
func captureRun(ctx context.Context, opts agentcore.Options, input string, emit func(db.Activity)) (string, harness.TerminalReason, error) {
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)
	return captureRunSession(ctx, s, input, emit)
}

// captureRunSession is captureRun over an existing session, so a caller can run
// multiple prompts on the SAME conversation (e.g. a settlement round that reuses
// the worker's accumulated context after the main run hit max_turns).
func captureRunSession(ctx context.Context, s *agentcore.Session, input string, emit func(db.Activity)) (string, harness.TerminalReason, error) {
	return captureEvents(ctx, s.Prompt(ctx, input), emit)
}

// captureSettleSession drives a forced wrap-up pass (Session.Settle) on the same
// conversation and emits its steps like any other run. Used after the hard ctx
// backstop killed the main run: the terminal reason is `reason` (e.g. timeout),
// not "completed", and only the non-disabled (write-back) tools are visible.
func captureSettleSession(ctx context.Context, s *agentcore.Session, input string, disabledTools []string, maxTurns int, reason harness.TerminalReason, emit func(db.Activity)) (string, harness.TerminalReason, error) {
	return captureEvents(ctx, s.Settle(ctx, input, disabledTools, maxTurns, reason), emit)
}

// captureEvents is the shared event loop behind captureRunSession/SettleSession.
func captureEvents(ctx context.Context, seq iter.Seq2[harness.Event, error], emit func(db.Activity)) (string, harness.TerminalReason, error) {
	var reason harness.TerminalReason
	rec := func(r db.Activity) {
		if emit != nil {
			emit(r)
		}
	}
	toolNames := map[string]string{} // tool_use id -> name, to label results

	var tbuf strings.Builder
	var tkind string
	flush := func() {
		if tbuf.Len() == 0 {
			return
		}
		s := strings.TrimSpace(tbuf.String())
		k := tkind
		tbuf.Reset()
		tkind = ""
		if s != "" {
			rec(db.Activity{Kind: k, Summary: firstLine(s, 200), Detail: s})
		}
	}
	addDelta := func(kind, text string) {
		if text == "" {
			return
		}
		if tkind != "" && tkind != kind {
			flush()
		}
		tkind = kind
		tbuf.WriteString(text)
	}

	var finalText string
	var rerr error
	for ev, err := range seq {
		if err != nil {
			flush()
			if ctx.Err() != nil { // ctx cancelled = manual stop / hard deadline
				rec(db.Activity{Kind: "result", Summary: ctxStopSummary(ctx.Err())})
				return finalText, reason, ctx.Err()
			}
			rec(db.Activity{Kind: "result", IsError: true, Summary: "执行出错: " + err.Error(), Detail: err.Error()})
			return finalText, reason, err
		}
		switch ev.Kind {
		case harness.KindToolUse:
			if ev.ToolUse == nil {
				continue
			}
			flush()
			toolNames[ev.ToolUse.ID] = ev.ToolUse.Name
			in := string(ev.ToolUse.Input)
			rec(db.Activity{Kind: "tool_use", Tool: ev.ToolUse.Name, ToolUseID: ev.ToolUse.ID,
				Summary: ev.ToolUse.Name + " " + firstLine(in, 200), Detail: in})
		case harness.KindToolResult:
			if ev.ToolResult == nil {
				continue
			}
			flush()
			out := blocksText(ev.ToolResult.Content)
			rec(db.Activity{Kind: "tool_result", Tool: toolNames[ev.ToolResult.ToolUseID], ToolUseID: ev.ToolResult.ToolUseID,
				IsError: ev.ToolResult.IsError, Summary: firstLine(out, 200), Detail: out})
		case harness.KindText:
			addDelta("text", ev.Text)
		case harness.KindThinking:
			addDelta("thinking", ev.Text)
		case harness.KindUsage:
			// live cumulative token usage (per model turn). Emitted as a non-rendered
			// "usage" activity carrying only the token fields; the UI uses the latest
			// one for a running session's live token count. Don't flush() here — the
			// buffered final-answer text must stay for the KindResult de-dup.
			if ev.Usage != nil {
				u := *ev.Usage
				rec(db.Activity{Kind: "usage",
					InputTokens: &u.InputTokens, OutputTokens: &u.OutputTokens,
					CacheReadTokens: &u.CacheReadTokens, CacheWriteTokens: &u.CacheWriteTokens})
			}
		case harness.KindResult:
			if ev.Terminal != nil {
				finalText = ev.Terminal.Text
				reason = ev.Terminal.Reason
				// the buffered tail text usually equals Terminal.Text (final answer);
				// drop it to avoid a duplicate record, the result row carries it.
				if tkind == "text" && strings.TrimSpace(tbuf.String()) == strings.TrimSpace(ev.Terminal.Text) {
					tbuf.Reset()
					tkind = ""
				}
				flush() // flush any trailing thinking / non-final text
				sum := ev.Terminal.Text
				if sum == "" {
					switch ev.Terminal.Reason {
					case harness.ReasonAbortedTools, harness.ReasonAbortedStreaming:
						// interruption (task paused / worker killed / backend restart /
						// per-run hard deadline), not a real completion — the intent is
						// re-queued (open), stopped, or force-settled by the caller.
						sum = "（运行被中断：任务暂停 / 终止 / 重启所致；未完成）"
						if ev.Terminal.Err != nil && errors.Is(ev.Terminal.Err, context.DeadlineExceeded) {
							// 单次 run 的硬墙钟兜底先到：主流程中断，调用方会紧接着
							// 用新 ctx 强制跑一轮收尾写回（见 worker.Execute）。
							sum = "（单次运行达墙钟硬上限，主流程已中断）"
						}
					case harness.ReasonTimeout, harness.ReasonMaxTurns:
						// budget hit → settlement ran (facts/assets were written back);
						// only the trailing text summary is missing, not a failure.
						sum = "（达运行预算上限，已收尾写回事实；本次无文字总结，终态: " + string(ev.Terminal.Reason) + "）"
					default:
						sum = "（无总结，终态: " + string(ev.Terminal.Reason) + "）"
					}
				}
				u := ev.Terminal.Usage // cumulative token usage for this session
				// A per-run hard deadline is a host budget, not an execution failure:
				// don't paint the interruption row red (the caller force-settles next).
				isErr := ev.Terminal.Err != nil && !errors.Is(ev.Terminal.Err, context.DeadlineExceeded)
				rec(db.Activity{Kind: "result", IsError: isErr,
					Summary: firstLine(sum, 400), Detail: sum,
					InputTokens: &u.InputTokens, OutputTokens: &u.OutputTokens,
					CacheReadTokens: &u.CacheReadTokens, CacheWriteTokens: &u.CacheWriteTokens})
				if ev.Terminal.Err != nil {
					rerr = ev.Terminal.Err
				}
			}
		}
	}
	flush() // safety: any unflushed text if the stream ended without KindResult
	return finalText, reason, rerr
}

// ctxStopSummary renders the note for a run whose ctx was cancelled: a deadline is
// the per-run wall-clock backstop (the caller may force a wrap-up right after),
// anything else is a manual/engine stop.
func ctxStopSummary(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "（单次运行达墙钟硬上限，主流程已中断）"
	}
	return "（已手动停止本次运行）"
}

// blocksText concatenates the text of a tool-result's content blocks.
func blocksText(blocks []llm.ContentBlock) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == llm.BlockText && bl.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// firstLine returns a single-line, length-capped preview for the summary column.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
