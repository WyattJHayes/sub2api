package openai_ws_v2

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestRelayCacheWriteTTLReachesTurnCallbackAndResult(t *testing.T) {
	client := newPassthroughTestFrameConn(nil, false)
	upstream := newPassthroughTestFrameConn([]passthroughTestFrame{{
		msgType: coderws.MessageText,
		payload: []byte(`{"type":"response.completed","response":{"id":"resp_ttl","usage":{"input_tokens":12,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}}}`),
	}}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var turns []Usage
	result, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"gpt-6-luna","input":[]}`), RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn.Usage) },
	})
	require.Nil(t, exit)
	require.Len(t, turns, 1)
	require.Equal(t, 3, turns[0].CacheCreation30mInputTokens)
	require.Equal(t, 7, turns[0].CacheCreation12hInputTokens)
	require.Equal(t, turns[0], result.Usage)
}

func TestRelayCacheWriteTTLAccounting(t *testing.T) {
	for _, path := range []string{"input_tokens_details", "prompt_tokens_details"} {
		t.Run(path, func(t *testing.T) {
			state := &relayState{}
			parse := func(event, details string) {
				body := fmt.Sprintf(`{"type":%q,"response":{"usage":{"input_tokens":12,"output_tokens":1,%q:{"cache_write_tokens":10,"cache_write_tokens_details":%s}}}}`, event, path, details)
				parseUsageAndAccumulate(state, []byte(body), event, nil)
			}
			parse("response.in_progress", `{"ttl_30m_tokens":3,"ttl_12h_tokens":7}`)
			parse("response.completed", `{"ttl_30m_tokens":3,"ttl_12h_tokens":7}`)
			first := finalizeRelayTurnUsage(state)
			require.Equal(t, 3, first.CacheCreation30mInputTokens)
			require.Equal(t, 7, first.CacheCreation12hInputTokens)
			// A second turn adds once, rather than adding progressive snapshots.
			parse("response.completed", `{"ttl_30m_tokens":0,"ttl_12h_tokens":10}`)
			finalizeRelayTurnUsage(state)
			require.Equal(t, 20, state.usage.CacheCreationInputTokens)
			require.Equal(t, 3, state.usage.CacheCreation30mInputTokens)
			require.Equal(t, 17, state.usage.CacheCreation12hInputTokens)
		})
	}
}

func TestRelayCacheWriteTTLInvalidDetailsClearProgressiveBuckets(t *testing.T) {
	for _, details := range []string{`null`, `{}`, `{"ttl_12h_tokens":10}`, `{"ttl_30m_tokens":0,"ttl_12h_tokens":9}`, `{"ttl_30m_tokens":-1,"ttl_12h_tokens":11}`, `{"ttl_30m_tokens":0.5,"ttl_12h_tokens":9.5}`, `{"ttl_30m_tokens":"0","ttl_12h_tokens":"10"}`} {
		t.Run(details, func(t *testing.T) {
			state := &relayState{}
			parseUsageAndAccumulate(state, []byte(`{"usage":{"input_tokens":12,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":7}}}}`), "response.in_progress", nil)
			parseUsageAndAccumulate(state, []byte(`{"usage":{"input_tokens":12,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":10,"cache_write_tokens_details":`+details+`}}}`), "response.in_progress", nil)
			got := finalizeRelayTurnUsage(state)
			require.Equal(t, 10, got.CacheCreationInputTokens)
			require.Zero(t, got.CacheCreation30mInputTokens)
			require.Zero(t, got.CacheCreation12hInputTokens)
		})
	}
}
