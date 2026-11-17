package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session reuse. Chat clients resend the whole transcript every turn, while Antigravity only hits its
// prompt cache inside one session (a fresh session with an identical prompt reads 0 cached tokens).
// After a turn the session stays open, keyed by a fingerprint of the transcript the client will send
// back; the next request whose history matches continues it with only the new messages.

const (
	acpConvIdleTTL = 30 * time.Minute
	acpConvMax     = 256
)

type acpConv struct {
	client    *acpClient
	sessionID string
	lastUsed  time.Time
}

var acpConvs = struct {
	sync.Mutex
	m map[string]*acpConv
}{m: map[string]*acpConv{}}

// takeConv removes the conversation stored under key, so two requests never share a live turn.
func takeConv(key string) *acpConv {
	acpConvs.Lock()
	c := acpConvs.m[key]
	delete(acpConvs.m, key)
	acpConvs.Unlock()
	if c == nil || !c.client.alive() {
		return nil
	}
	return c
}

func putConv(key string, c *acpConv) {
	c.lastUsed = time.Now()
	var evict []*acpConv
	acpConvs.Lock()
	if old := acpConvs.m[key]; old != nil && old != c {
		evict = append(evict, old)
	}
	acpConvs.m[key] = c
	for k, v := range acpConvs.m {
		if v != c && (time.Since(v.lastUsed) > acpConvIdleTTL || !v.client.alive()) {
			evict = append(evict, v)
			delete(acpConvs.m, k)
		}
	}
	for len(acpConvs.m) > acpConvMax {
		oldestKey := ""
		var oldest *acpConv
		for k, v := range acpConvs.m {
			if oldest == nil || v.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest = k, v
			}
		}
		delete(acpConvs.m, oldestKey)
		evict = append(evict, oldest)
	}
	acpConvs.Unlock()
	for _, v := range evict {
		v := v
		safeGo("closeConv", func() { v.client.closeSession(v.sessionID) })
	}
}

func dropAllConvs() {
	acpConvs.Lock()
	acpConvs.m = map[string]*acpConv{}
	acpConvs.Unlock()
}

// acpPlan carries both prompts for a request: the full transcript for a fresh session and, when a
// stored session matches the history, just the new messages for that session.
type acpPlan struct {
	full     []acpPromptBlock
	follow   []acpPromptBlock
	reuse    *acpConv
	scope    string
	tools    []json.RawMessage
	messages []chatMessage
}

func convScope(cfg pluginConfig, opts agyOptions) string {
	return cfg.AccountHome + "\x00" + opts.NativeModel + "\x00" + opts.ReasoningEffort
}

func planACP(req rpcExecutorRequest, opts agyOptions, cfg pluginConfig) (acpPlan, error) {
	full, err := buildACPBlocks(req, opts, cfg)
	if err != nil {
		return acpPlan{}, err
	}
	chatReq, err := decodeChatRequest(req.Payload)
	if err != nil {
		return acpPlan{}, err
	}
	plan := acpPlan{full: full, scope: convScope(cfg, opts), tools: chatReq.Tools, messages: chatReq.Messages}
	last := lastAssistantIndex(chatReq.Messages)
	if last < 0 || last == len(chatReq.Messages)-1 {
		return plan, nil
	}
	conv := takeConv(convKey(plan.scope, plan.tools, chatReq.Messages[:last+1]))
	if conv == nil {
		return plan, nil
	}
	var images []acpPromptBlock
	text, err := buildFollowUpPrompt(chatReq.Messages[last+1:], len(chatReq.Tools) > 0, cfg.Workdir, &images)
	if err != nil {
		safeGo("closeConv", func() { conv.client.closeSession(conv.sessionID) })
		return plan, nil
	}
	plan.reuse = conv
	plan.follow = append([]acpPromptBlock{{"type": "text", "text": text}}, images...)
	return plan, nil
}

// retainConv keeps a finished turn's session for the request that will carry this reply as history.
func (p acpPlan) retainConv(conv *acpConv, reply string, toolCalls []openAIToolCall, isToolCall bool) {
	if conv == nil {
		return
	}
	assistant := chatMessage{Role: "assistant"}
	if isToolCall {
		assistant.Content = json.RawMessage("null")
		assistant.ToolCalls, _ = json.Marshal(toolCalls)
	} else {
		assistant.Content, _ = json.Marshal(reply)
	}
	history := append(append([]chatMessage(nil), p.messages...), assistant)
	putConv(convKey(p.scope, p.tools, history), conv)
}

func lastAssistantIndex(messages []chatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			return i
		}
	}
	return -1
}

func buildFollowUpPrompt(messages []chatMessage, hasTools bool, workdir string, images *[]acpPromptBlock) (string, error) {
	if !hasTools && len(messages) == 1 && canonRole(messages[0].Role) == "user" {
		return messageContent(messages[0].Content, workdir, images)
	}
	var b strings.Builder
	for i, msg := range messages {
		content, err := messageContent(msg.Content, workdir, images)
		if err != nil {
			return "", fmt.Errorf("message %d: %w", i+1, err)
		}
		b.WriteString("--- message role=" + canonRole(msg.Role))
		if msg.Name != "" {
			b.WriteString(" name=" + msg.Name)
		}
		if msg.ToolCallID != "" {
			b.WriteString(" tool_call_id=" + msg.ToolCallID)
		}
		b.WriteString(" ---\n")
		if content != "" {
			b.WriteString(strings.TrimRight(content, "\n") + "\n")
		}
	}
	if hasTools {
		b.WriteString("\n[The client-side tools listed earlier in this conversation are still available. To call one, reply ONLY with a ```tool_calls JSON block as instructed earlier.]\n")
	}
	return b.String(), nil
}

// convKey fingerprints a transcript. It ignores what proxies rewrite on the way back (tool call IDs,
// whitespace, argument key order, text-vs-block content) so a client's echo matches what was sent.
func convKey(scope string, tools []json.RawMessage, messages []chatMessage) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(scope)
	for _, t := range tools {
		write(canonJSON(t))
	}
	write("\x1e")
	for _, m := range messages {
		write(canonRole(m.Role))
		write(strings.Join(strings.Fields(canonContent(m.Content)), " "))
		for _, call := range canonToolCalls(m.ToolCalls) {
			write(call)
		}
		write("\x1e")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonRole(role string) string {
	switch r := strings.ToLower(strings.TrimSpace(role)); r {
	case "", "user":
		return "user"
	case "developer":
		return "system"
	default:
		return r
	}
}

func canonContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, block := range blocks {
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
			continue
		}
		encoded, _ := json.Marshal(block)
		sum := sha256.Sum256(encoded)
		parts = append(parts, "[block "+hex.EncodeToString(sum[:8])+"]")
	}
	return strings.Join(parts, "\n")
}

func canonToolCalls(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var calls []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return []string{canonJSON(raw)}
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Function.Name+"("+canonJSON(json.RawMessage(compactToolArguments(c.Function.Arguments)))+")")
	}
	sort.Strings(out)
	return out
}

// canonJSON re-encodes JSON with sorted object keys; invalid JSON is returned trimmed.
func canonJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return strings.TrimSpace(string(raw))
	}
	out, _ := json.Marshal(v)
	return string(out)
}
