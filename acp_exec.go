package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	acpImageMaxBytes     = 20 << 20
	acpImageFetchTimeout = 30 * time.Second
	acpDiscoverTimeout   = 20 * time.Second
)

// useACP reports whether a request runs on the resident agy. Resuming an agy conversation by ID
// is only supported by print mode, so those requests keep using it.
func useACP(cfg pluginConfig, opts agyOptions) bool {
	return cfg.Mode == modeACP && strings.TrimSpace(opts.ConversationID) == ""
}

// acpImageBlock turns a data URL or http(s) URL into an ACP image block. Local paths are refused:
// in ACP mode nothing on the CLIProxyAPI host is readable through a request.
func acpImageBlock(ref string) (acpPromptBlock, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "data:"):
		comma := strings.IndexByte(ref, ',')
		if comma < 0 || !strings.Contains(ref[:comma], ";base64") {
			return nil, fmt.Errorf("unsupported data URL: base64 encoding is required")
		}
		mime := strings.TrimPrefix(strings.SplitN(ref[:comma], ";", 2)[0], "data:")
		data := ref[comma+1:]
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, fmt.Errorf("invalid base64 image: %w", err)
		}
		return acpPromptBlock{"type": "image", "mimeType": nonEmpty(mime, "image/png"), "data": data}, nil
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		ctx, cancel := context.WithTimeout(context.Background(), acpImageFetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetching image: HTTP %d", resp.StatusCode)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, acpImageMaxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("fetching image: %w", err)
		}
		if len(raw) > acpImageMaxBytes {
			return nil, fmt.Errorf("image larger than %d bytes", acpImageMaxBytes)
		}
		mime := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
		if !strings.HasPrefix(mime, "image/") {
			mime = http.DetectContentType(raw)
		}
		return acpPromptBlock{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(raw)}, nil
	default:
		return nil, fmt.Errorf("only data: and http(s) image URLs are accepted in acp mode")
	}
}

func buildACPBlocks(req rpcExecutorRequest, opts agyOptions, cfg pluginConfig) ([]acpPromptBlock, error) {
	var images []acpPromptBlock
	prompt, err := buildPromptWith(req.Payload, opts.ConversationID, cfg.Workdir, &images)
	if err != nil {
		return nil, err
	}
	blocks := []acpPromptBlock{{"type": "text", "text": prompt}}
	return append(blocks, images...), nil
}

func executeNonStreamACP(req rpcExecutorRequest, cfg pluginConfig, opts agyOptions) (agyResult, error) {
	plan, err := planACP(req, opts, cfg)
	if err != nil {
		return agyResult{}, err
	}
	result, conv, err := runACP(cfg, opts, plan, nil)
	if err != nil {
		return agyResult{}, err
	}
	toolCalls, isToolCall := extractToolCalls(result.Response)
	plan.retainConv(conv, result.Response, toolCalls, isToolCall)
	return result, nil
}

// executeStreamACP mirrors executeStream's SSE framing, fed by ACP agent_message_chunk updates.
func executeStreamACP(req rpcExecutorRequest, cfg pluginConfig, opts agyOptions) {
	streamID := req.StreamID
	plan, err := planACP(req, opts, cfg)
	if err != nil {
		closePluginStream(streamID, err)
		return
	}

	out := newChatStream(req, "")
	var streamed strings.Builder
	emittedLen := 0
	isToolCallStream := false
	toolDecisionPending := opts.HasTools
	onDelta := func(delta string) error {
		streamed.WriteString(delta)
		if toolDecisionPending {
			switch toolCallPrefixState(streamed.String()) {
			case toolPrefixMatch:
				isToolCallStream = true
				toolDecisionPending = false
			case toolPrefixNone:
				toolDecisionPending = false
			}
		}
		if isToolCallStream || toolDecisionPending {
			return nil
		}
		pending := streamed.String()[emittedLen:]
		emittedLen = streamed.Len()
		return out.emit(map[string]any{"content": pending}, nil, nil)
	}

	result, conv, err := runACP(cfg, opts, plan, onDelta)
	if err == errACPClientGone {
		return
	}
	if err != nil {
		closePluginStream(streamID, err)
		return
	}

	fullResponse := nonEmpty(result.Response, streamed.String())
	toolCalls, isToolCall := extractToolCalls(fullResponse)
	isToolCall = isToolCall && opts.HasTools
	plan.retainConv(conv, fullResponse, toolCalls, isToolCall)
	if !isToolCall && strings.TrimSpace(fullResponse) == "" {
		closePluginStream(streamID, emptyResponseError())
		return
	}
	if isToolCall {
		_ = out.emit(map[string]any{"tool_calls": toolCalls}, nil, nil)
		finish := "tool_calls"
		_ = out.emit(map[string]any{}, &finish, nil)
		if streamWantsUsage(req) {
			_ = out.emit(map[string]any{}, nil, openAIUsage(result.Usage))
		}
		out.finish()
		return
	}

	if streamed.Len() > emittedLen {
		if err := out.emit(map[string]any{"content": streamed.String()[emittedLen:]}, nil, nil); err != nil {
			return
		}
	}
	finish := "stop"
	if err := out.emit(map[string]any{}, &finish, nil); err != nil {
		return
	}
	if streamWantsUsage(req) {
		if err := out.emit(map[string]any{}, nil, openAIUsage(result.Usage)); err != nil {
			return
		}
	}
	out.finish()
}

// discoverModelsACP lists the models of cfg's account from its resident agy.
func discoverModelsACP(cfg pluginConfig) ([]modelInfo, map[string]string, error) {
	c := livePooledACP(cfg)
	switch {
	case c != nil:
	case cfg.AccountHome != "" || processHomeServesRequests():
		var err error
		if c, err = acquireACP(cfg); err != nil {
			return nil, nil, err
		}
	default:
		temp, err := startACP(cfg, "discovery")
		if err != nil {
			return nil, nil, err
		}
		defer temp.shutdown(errors.New("discovery finished"))
		c = temp
	}
	c.busy.Add(1)
	defer c.busy.Add(-1)
	ctx, cancel := context.WithTimeout(context.Background(), acpDiscoverTimeout)
	defer cancel()
	cwd := nonEmpty(cfg.Workdir, ".")
	set, err := c.listModels(ctx, cwd)
	if err != nil {
		return nil, nil, err
	}
	var models []modelInfo
	mapping := map[string]string{}
	seen := map[string]bool{}
	add := func(o acpModelOption, description string) {
		id := "agy/" + o.ID
		if seen[id] {
			return
		}
		seen[id] = true
		name := nonEmpty(o.Name, humanizeModelName(o.ID))
		models = append(models, modelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    "google-antigravity-cli",
			DisplayName:                name,
			Name:                       name,
			Description:                description,
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		})
		mapping[id] = o.ID
	}
	for _, o := range set.Bases {
		add(o, "Discovered from the resident agy acp session with configurable reasoning effort.")
	}
	for _, o := range set.Variants {
		add(o, "Discovered from the resident agy acp session.")
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("agy acp reported no models")
	}
	return models, mapping, nil
}

func processHomeServesRequests() bool {
	for _, home := range enabledAgyHomes(currentAuthDir()) {
		if home == "" {
			return true
		}
	}
	return false
}

// safeGo runs fn on a goroutine that can never take the CLIProxyAPI process down with a panic.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("cliproxy-antigravity: %s panic: %v", name, r)
			}
		}()
		fn()
	}()
}
