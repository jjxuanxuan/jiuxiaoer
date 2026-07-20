package auth

type SendCodeReq struct {
	Phone string `json:"phone" binding:"required"`
}

type SmsLoginReq struct {
	Phone string `json:"phone" binding:"required"`
	Code  string `json:"code" binding:"required,len=6"`
}

type WeChatLoginReq struct {
	Code     string `json:"code" binding:"required,min=4,max=256"`
	DeviceID string `json:"device_id" binding:"omitempty,max=128"`
}

type PhoneBindReq struct {
	PhoneCode string `json:"phone_code" binding:"required,min=4,max=256"`
}

type PasswordLoginReq struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Password string `json:"password" binding:"required,min=6,max=128"`
}

type RefreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type TokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	AccountType  string `json:"account_type"`
	AccountID    string `json:"account_id"`
	Profile      any    `json:"profile"`
}

type WeChatLoginResp struct {
	*TokenResp
	PhoneBound bool `json:"phone_bound"`
}

type PhoneBindResp struct {
	CustomerID string `json:"customer_id"`
	Phone      string `json:"phone"`
}
