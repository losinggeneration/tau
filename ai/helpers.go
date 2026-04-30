package ai

import (
	"context"
	"fmt"
)

// completeViaStream implements Completer.Complete by collecting the
// terminal StreamEvent from the supplied Streamer. A Done event supplies
// the returned AssistantMessage; a StreamError event (which providers
// emit via emitTerminalStreamError and pair with Stream returning nil)
// overwrites any prior result. This matches the historical per-provider
// Complete semantics and is the centralized implementation every
// standard provider's Complete method delegates to. Bedrock diverges
// intentionally (it has a different error-shape contract) and continues
// to maintain its own inline handler.
func completeViaStream(ctx context.Context, s Streamer, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	var result AssistantMessage
	handler := func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case Done:
			result = e.Message
		case StreamError:
			result = e.Msg
		}
	}
	if err := s.Stream(ctx, model, prompt, handler, opts...); err != nil {
		return nil, err
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
// options (notably a MaxTokens default of min(model.MaxTokens, 32000)).
func StreamSimpleWithModel(ctx context.Context, model *Model, prompt *Context, handler EventHandler, simple *SimpleStreamOptions) error {
	if model == nil {
		return fmt.Errorf("stream simple: model is nil")
	}
	return StreamWithModel(ctx, model, prompt, handler, BuildBaseOptions(model, simple)...)
}

// StreamSimpleProvider streams using the first known model for a provider.
func StreamSimpleProvider(ctx context.Context, provider string, prompt *Context, handler EventHandler, simple *SimpleStreamOptions) error {
	models := ListModels(provider)
	if len(models) == 0 {
		return fmt.Errorf("stream simple: no models available for provider: %s", provider)
	}
	return StreamWithModel(ctx, models[0], prompt, handler, BuildBaseOptions(models[0], simple)...)
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
// options (notably a MaxTokens default of min(model.MaxTokens, 32000)).
func CompleteSimpleWithModel(ctx context.Context, model *Model, prompt *Context, simple *SimpleStreamOptions) (*AssistantMessage, error) {
	if model == nil {
		return nil, fmt.Errorf("complete simple: model is nil")
	}
	return CompleteWithModel(ctx, model, prompt, BuildBaseOptions(model, simple)...)
}

// CompleteSimpleProvider completes using the first known model for a provider.
func CompleteSimpleProvider(ctx context.Context, provider string, prompt *Context, simple *SimpleStreamOptions) (*AssistantMessage, error) {
	models := ListModels(provider)
	if len(models) == 0 {
		return nil, fmt.Errorf("complete simple: no models available for provider: %s", provider)
	}
	return CompleteWithModel(ctx, models[0], prompt, BuildBaseOptions(models[0], simple)...)
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
