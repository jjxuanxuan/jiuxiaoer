package auth

import (
	"context"
	"errors"
)

var (
	ErrWeChatCodeInvalid         = errors.New("wechat code is invalid")
	ErrWeChatProviderUnavailable = errors.New("wechat provider is unavailable")
)

type WeChatIdentityResult struct {
	AppID          string
	OpenID         string
	UnionID        string
	SessionKeyHash string
}

type WeChatProvider interface {
	ExchangeCode(ctx context.Context, code string) (WeChatIdentityResult, error)
	ResolvePhone(ctx context.Context, phoneCode string) (string, error)
}
