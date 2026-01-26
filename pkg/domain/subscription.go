package domain

import "time"

type Subscription struct {
	ID                 string    `json:"id"`
	CallbackURL        string    `json:"callbackUrl"`
	EventTypes         []Command `json:"eventTypes"`
	DeliveryMode       string    `json:"deliveryMode"`
	GroupID            string    `json:"groupId,omitempty"`
	MinIntervalSeconds int       `json:"minIntervalSeconds"`
	ExpiresAt          time.Time `json:"expiresAt"`
	CreatedAt          time.Time `json:"createdAt"`
}
