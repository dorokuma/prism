package main

import "github.com/dorokuma/prism/internal/stream"

// translateChatStreamToResponses reads a chat completion SSE stream from body,
// translates each chunk into Responses API SSE events, and writes them to w.
var translateChatStreamToResponses = stream.TranslateChatStreamToResponses

// ErrEmptyUpstreamStream is returned when the chat completion stream has no model output.
var ErrEmptyUpstreamStream = stream.ErrEmptyUpstreamStream
