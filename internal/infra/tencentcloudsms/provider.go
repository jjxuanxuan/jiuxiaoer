package tencentcloudsms

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	sms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20210111"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

const successCode = "Ok"

type client interface {
	SendSmsWithContext(context.Context, *sms.SendSmsRequest) (*sms.SendSmsResponse, error)
}

// Provider 使用腾讯云短信 API 3.0（2021-01-11）发送登录验证码。
// 配置的模板必须按顺序包含验证码和有效分钟数两个变量。
type Provider struct {
	client     client
	sdkAppID   string
	signName   string
	templateID string
}

// DeliveryError 表示请求已被 API 接收，但被短信产品拒绝。
// 错误中只保留稳定的服务商代码，绝不包含手机号和凭据。
type DeliveryError struct {
	Code string
}

func (e *DeliveryError) Error() string {
	return "Tencent Cloud SMS delivery rejected: " + e.Code
}

func New(cfg config.SMSConfig) (auth.SMSProvider, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Provider != "tencentcloud" {
		return nil, fmt.Errorf("unsupported SMS provider %q", cfg.Provider)
	}
	if cfg.Region == "" || cfg.SecretID == "" || cfg.SecretKey == "" || cfg.SDKAppID == "" || cfg.SignName == "" || cfg.TemplateID == "" {
		return nil, errors.New("incomplete Tencent Cloud SMS configuration")
	}

	clientProfile := profile.NewClientProfile()
	clientProfile.HttpProfile.Endpoint = cfg.Endpoint
	timeoutSeconds := int(cfg.HTTPTimeout.Seconds())
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	clientProfile.HttpProfile.ReqTimeout = timeoutSeconds

	smsClient, err := sms.NewClient(common.NewCredential(cfg.SecretID, cfg.SecretKey), cfg.Region, clientProfile)
	if err != nil {
		return nil, fmt.Errorf("initialize Tencent Cloud SMS client: %w", err)
	}
	return &Provider{
		client:     smsClient,
		sdkAppID:   cfg.SDKAppID,
		signName:   cfg.SignName,
		templateID: cfg.TemplateID,
	}, nil
}

func (p *Provider) SendVerificationCode(ctx context.Context, phone, code string, ttl time.Duration) error {
	validMinutes := int(ttl / time.Minute)
	if validMinutes < 1 {
		validMinutes = 1
	}
	request := sms.NewSendSmsRequest()
	request.PhoneNumberSet = []*string{common.StringPtr("+86" + phone)}
	request.SmsSdkAppId = common.StringPtr(p.sdkAppID)
	request.SignName = common.StringPtr(p.signName)
	request.TemplateId = common.StringPtr(p.templateID)
	request.TemplateParamSet = []*string{
		common.StringPtr(code),
		common.StringPtr(strconv.Itoa(validMinutes)),
	}

	response, err := p.client.SendSmsWithContext(ctx, request)
	if err != nil {
		return fmt.Errorf("Tencent Cloud SMS request failed: %w", err)
	}
	if response == nil || response.Response == nil || len(response.Response.SendStatusSet) != 1 || response.Response.SendStatusSet[0] == nil || response.Response.SendStatusSet[0].Code == nil {
		return errors.New("Tencent Cloud SMS returned an invalid response")
	}
	if code := *response.Response.SendStatusSet[0].Code; code != successCode {
		return &DeliveryError{Code: code}
	}
	return nil
}
