package tencentcloudsms

import (
	"context"
	"errors"
	"testing"
	"time"

	sms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20210111"
)

type fakeClient struct {
	request  *sms.SendSmsRequest
	response *sms.SendSmsResponse
	err      error
}

func (c *fakeClient) SendSmsWithContext(_ context.Context, request *sms.SendSmsRequest) (*sms.SendSmsResponse, error) {
	c.request = request
	return c.response, c.err
}

func TestSendVerificationCodeBuildsTencentCloudRequest(t *testing.T) {
	client := &fakeClient{response: sendResponse("Ok")}
	provider := &Provider{client: client, sdkAppID: "1400006666", signName: "酒小二", templateID: "1234567"}

	if err := provider.SendVerificationCode(context.Background(), "13800138000", "654321", 5*time.Minute); err != nil {
		t.Fatalf("send verification code: %v", err)
	}
	request := client.request
	if request == nil || len(request.PhoneNumberSet) != 1 || *request.PhoneNumberSet[0] != "+8613800138000" {
		t.Fatalf("unexpected phone request: %#v", request)
	}
	if *request.SmsSdkAppId != "1400006666" || *request.SignName != "酒小二" || *request.TemplateId != "1234567" {
		t.Fatalf("unexpected Tencent Cloud identifiers: %#v", request)
	}
	if len(request.TemplateParamSet) != 2 || *request.TemplateParamSet[0] != "654321" || *request.TemplateParamSet[1] != "5" {
		t.Fatalf("unexpected template params: %#v", request.TemplateParamSet)
	}
}

func TestSendVerificationCodeReturnsStableDeliveryError(t *testing.T) {
	client := &fakeClient{response: sendResponse("FailedOperation.TemplateIncorrectOrUnapproved")}
	provider := &Provider{client: client}

	err := provider.SendVerificationCode(context.Background(), "13800138000", "654321", 5*time.Minute)
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Code != "FailedOperation.TemplateIncorrectOrUnapproved" {
		t.Fatalf("unexpected delivery error: %v", err)
	}
}

func sendResponse(code string) *sms.SendSmsResponse {
	return &sms.SendSmsResponse{Response: &sms.SendSmsResponseParams{
		SendStatusSet: []*sms.SendStatus{{Code: &code}},
	}}
}
