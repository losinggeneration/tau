package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// completeViaStream collects the first terminal assistant event and waits for
// Stream to return so transport and cleanup errors remain observable.
func completeViaStream(ctx context.Context, s Streamer, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	var result AssistantMessage
	var terminal bool
	var mu sync.Mutex
	handler := timedEventHandler(func(_ context.Context, event StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		if terminal {
			return
		}
		switch e := event.(type) {
		case Done:
			terminal = true
			result = e.Message
		case StreamError:
			terminal = true
			result = e.Msg
		}
	})
	if err := s.Stream(ctx, model, prompt, handler, opts...); err != nil {
		return nil, err
	}
	if !terminal {
		return nil, fmt.Errorf("stream ended without a terminal assistant event")
	}
	return &result, nil
}

func resolveModelArg(modelArg any) (*Model, error) {
	switch v := modelArg.(type) {
	case *Model:
		if v == nil {
			return nil, fmt.Errorf("model is nil")
		}
		return v, nil
	case string:
		return ResolveModelID(v)
	default:
		return nil, fmt.Errorf("unsupported model argument type %T", modelArg)
	}
}

func resolveClient(model *Model, opts []Option) (Client, error) {
	o := applyOptions(opts)
	if o.client != nil {
		return o.client, nil
	}

	client, err := GetClient(model)
	if err != nil {
		return nil, err
	}

	return client, nil
}

// StreamWithModel calls Client.Stream with the provided model.
// It resolves the client through the API registry using model.API.
func StreamWithModel(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	client, err := resolveClient(model, opts)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	return client.Stream(ctx, model, prompt, handler, opts...)
}

// CompleteWithModel calls Client.Complete with the provided model.
// It resolves the client through the API registry using model.API.
func CompleteWithModel(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	client, err := resolveClient(model, opts)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	return client.Complete(ctx, model, prompt, opts...)
}

// StreamByID resolves a model specifier then calls Client.Stream.
func StreamByID(ctx context.Context, modelID string, prompt *Context, handler EventHandler, opts ...Option) error {
	model, err := ResolveModelID(modelID)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	return StreamWithModel(ctx, model, prompt, handler, opts...)
}

// CompleteByID resolves a model specifier then calls Client.Complete.
func CompleteByID(ctx context.Context, modelID string, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	model, err := ResolveModelID(modelID)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	return CompleteWithModel(ctx, model, prompt, opts...)
}

// Stream calls Client.Stream with either a *Model or a "provider/modelID"
// string. Prefer StreamWithModel or StreamByID in new Go code.
func Stream(ctx context.Context, modelArg any, prompt *Context, handler EventHandler, opts ...Option) error {
	model, err := resolveModelArg(modelArg)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	return StreamWithModel(ctx, model, prompt, handler, opts...)
}

// Complete calls Client.Complete with either a *Model or a "provider/modelID"
// string. Prefer CompleteWithModel or CompleteByID in new Go code.
func Complete(ctx context.Context, modelArg any, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	model, err := resolveModelArg(modelArg)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	return CompleteWithModel(ctx, model, prompt, opts...)
}

// StreamSimpleWithModel streams using a concrete model and simple defaults
// configured via the provided SimpleStreamOptions. Passing nil applies default
// options (notably a MaxTokens default of the model cap clamped to the
// context room).
func StreamSimpleWithModel(ctx context.Context, model *Model, prompt *Context, handler EventHandler, simple *SimpleStreamOptions) error {
	if model == nil {
		return fmt.Errorf("stream simple: model is nil")
	}
	return StreamWithModel(ctx, model, prompt, handler, BuildBaseOptions(model, prompt, simple)...)
}

// StreamSimpleProvider streams using the first known model for a provider.
func StreamSimpleProvider(ctx context.Context, provider string, prompt *Context, handler EventHandler, simple *SimpleStreamOptions) error {
	models := ListModels(provider)
	if len(models) == 0 {
		return fmt.Errorf("stream simple: no models available for provider: %s", provider)
	}
	return StreamWithModel(ctx, models[0], prompt, handler, BuildBaseOptions(models[0], prompt, simple)...)
}

// StreamSimple streams using either a *Model directly or the first known model
// for a provider string. Prefer StreamSimpleWithModel or StreamSimpleProvider in new Go code.
func StreamSimple(ctx context.Context, modelOrProvider any, prompt *Context, handler EventHandler, simple *SimpleStreamOptions) error {
	if model, ok := modelOrProvider.(*Model); ok {
		return StreamSimpleWithModel(ctx, model, prompt, handler, simple)
	}

	provider, ok := modelOrProvider.(string)
	if !ok {
		return fmt.Errorf("stream simple: unsupported model argument type %T", modelOrProvider)
	}
	return StreamSimpleProvider(ctx, provider, prompt, handler, simple)
}

// CompleteSimpleWithModel completes using a concrete model and simple defaults
// configured via the provided SimpleStreamOptions. Passing nil applies default
// options (notably a MaxTokens default of the model cap clamped to the
// context room).
func CompleteSimpleWithModel(ctx context.Context, model *Model, prompt *Context, simple *SimpleStreamOptions) (*AssistantMessage, error) {
	if model == nil {
		return nil, fmt.Errorf("complete simple: model is nil")
	}
	return CompleteWithModel(ctx, model, prompt, BuildBaseOptions(model, prompt, simple)...)
}

// CompleteSimpleProvider completes using the first known model for a provider.
func CompleteSimpleProvider(ctx context.Context, provider string, prompt *Context, simple *SimpleStreamOptions) (*AssistantMessage, error) {
	models := ListModels(provider)
	if len(models) == 0 {
		return nil, fmt.Errorf("complete simple: no models available for provider: %s", provider)
	}
	return CompleteWithModel(ctx, models[0], prompt, BuildBaseOptions(models[0], prompt, simple)...)
}

// CompleteSimple completes using either a *Model directly or the first known
// model for a provider string. Prefer CompleteSimpleWithModel or CompleteSimpleProvider in new Go code.
func CompleteSimple(ctx context.Context, modelOrProvider any, prompt *Context, simple *SimpleStreamOptions) (*AssistantMessage, error) {
	if model, ok := modelOrProvider.(*Model); ok {
		return CompleteSimpleWithModel(ctx, model, prompt, simple)
	}

	provider, ok := modelOrProvider.(string)
	if !ok {
		return nil, fmt.Errorf("complete simple: unsupported model argument type %T", modelOrProvider)
	}
	return CompleteSimpleProvider(ctx, provider, prompt, simple)
}

// TimedEventHandler times newly started responses and forwards events through
// the first terminal event. Existing durations and historical responses are kept.
func TimedEventHandler(handler EventHandler) EventHandler {
	return timedEventHandler(handler)
}

func timedEventHandler(handler EventHandler) EventHandler {
	started := time.Now()
	var mu sync.Mutex
	var settled bool
	return func(ctx context.Context, event StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		duration := math.Round(float64(time.Since(started)) / float64(time.Millisecond))
		switch terminal := event.(type) {
		case Done:
			settled = true
			if terminal.Message.DurationMs == nil && terminal.Message.Timestamp >= started.UnixMilli() {
				terminal.Message.DurationMs = new(duration)
			}
			event = terminal
		case StreamError:
			settled = true
			if terminal.Msg.DurationMs == nil && terminal.Msg.Timestamp >= started.UnixMilli() {
				terminal.Msg.DurationMs = new(duration)
			}
			event = terminal
		}
		handler(ctx, event)
	}
}

type cancelResponseBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *cancelResponseBody) Close() error {
	defer b.cancel(nil)
	return b.ReadCloser.Close()
}

func doHeaderRequest(req *http.Request, timeout time.Duration, label string) (*http.Response, error) {
	return doClientHeaderRequest(http.DefaultClient, req, timeout, label)
}

// doClientHeaderRequest bounds only the wait for response headers. A successful
// response's body releases the timed context when closed.
func doClientHeaderRequest(client *http.Client, req *http.Request, timeout time.Duration, label string) (*http.Response, error) {
	return headerTimeoutRoundTrip(client.Do, req, timeout, label)
}

// headerTimeoutTransport applies a header-only timeout to each round trip.
type headerTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
	label   string
}

func (t *headerTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return headerTimeoutRoundTrip(t.base.RoundTrip, req, t.timeout, t.label)
}

func headerTimeoutRoundTrip(do func(*http.Request) (*http.Response, error), req *http.Request, timeout time.Duration, label string) (*http.Response, error) {
	if timeout <= 0 {
		return do(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	deadline := fmt.Errorf("%s: response headers timed out: %w", label, context.DeadlineExceeded)
	timer := time.AfterFunc(timeout, func() { cancel(deadline) })
	resp, err := do(req.WithContext(ctx))
	timer.Stop()
	if err != nil {
		if errors.Is(context.Cause(ctx), deadline) {
			err = deadline
		}
		cancel(nil)
		return nil, err
	}
	resp.Body = &cancelResponseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
