package service

import openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"

func openAIUsageFromWSV2(usage openaiwsv2.Usage) OpenAIUsage {
	return OpenAIUsage{
		InputTokens:                 usage.InputTokens,
		ImageInputTokens:            usage.ImageInputTokens,
		OutputTokens:                usage.OutputTokens,
		CacheCreationInputTokens:    usage.CacheCreationInputTokens,
		CacheCreation30mInputTokens: usage.CacheCreation30mInputTokens,
		CacheCreation12hInputTokens: usage.CacheCreation12hInputTokens,
		CacheReadInputTokens:        usage.CacheReadInputTokens,
		ImageOutputTokens:           usage.ImageOutputTokens,
	}
}
