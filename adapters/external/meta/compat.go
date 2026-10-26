package meta

import (
	"github.com/buxee/alix-ai/adapters/external/chatcompat"
)

func compatConfig(cfg Config) chatcompat.Config {
	return chatcompat.Config{
		BaseURL: cfg.BaseURL, APIPrefix: cfg.APIPrefix, CredentialRef: cfg.CredentialRef,
		WireModel: cfg.Model, ReplyModel: cfg.ReplyModel, ReasoningEffort: cfg.ReasoningEffort, Profiles: cfg.Profiles,
		MaxContextToks: cfg.MaxContextToks, MaxOutputToks: cfg.MaxOutputToks, DefaultMaxOutputToks: cfg.DefaultMaxOutputToks,
		MaxTools: cfg.MaxTools, Prices: cfg.Prices, NoForcedToolChoice: cfg.NoForcedToolChoice,
		RequestTimeout: cfg.RequestTimeout, StreamTimeout: cfg.StreamTimeout, MaxResponseBytes: cfg.MaxResponseBytes, Retry: cfg.Retry,
	}
}

func compatOptions(c *Client) []chatcompat.Option {
	opts := []chatcompat.Option{chatcompat.WithClock(c.clk), chatcompat.WithLogger(c.log), chatcompat.WithSleeper(c.sleep)}
	return opts
}
