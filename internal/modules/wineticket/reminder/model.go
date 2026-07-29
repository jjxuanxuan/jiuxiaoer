package reminder

import (
	"time"
)

type Reminder struct {
	ID                uint64
	LotID             uint64
	OwnerCustomerID   uint64
	ExpiresAt         time.Time
	RemindDays        uint8
	Channel           string
	Status            string
	Attempts          uint
	ProviderMessageID *string
	LastErrorCode     *string
	LockedBy          *string
	LockedUntil       *time.Time
	ScheduledAt       time.Time
	SentAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (Reminder) TableName() string { return "wine_ticket_reminders" }

type NotificationSubscriptionConsent struct {
	ID                  uint64
	CustomerID          uint64
	Scene               string
	TemplateCode        string
	ConsentResult       string
	ProviderReceipt     *string
	Status              string
	ConsentedAt         time.Time
	ExpiresAt           *time.Time
	ClaimedByReminderID *uint64
	ClaimedAt           *time.Time
	RequestID           *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (NotificationSubscriptionConsent) TableName() string {
	return "notification_subscription_consents"
}
