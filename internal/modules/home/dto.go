package home

type SlotWriteReq struct {
	CityCode  string         `json:"city_code"`
	SlotType  string         `json:"slot_type" binding:"required"`
	SlotKey   string         `json:"slot_key" binding:"required,max=64"`
	Title     string         `json:"title" binding:"required,max=128"`
	Payload   map[string]any `json:"payload" binding:"required"`
	StartAt   string         `json:"start_at"`
	EndAt     string         `json:"end_at"`
	SortOrder int            `json:"sort_order"`
	Version   uint32         `json:"version,omitempty"`
}

type SlotStatusReq struct {
	Status  string `json:"status" binding:"required,oneof=draft published disabled"`
	Version uint32 `json:"version" binding:"required,min=1"`
}

type SlotDTO struct {
	ID        string         `json:"id"`
	CityCode  string         `json:"city_code,omitempty"`
	SlotType  string         `json:"slot_type"`
	SlotKey   string         `json:"slot_key"`
	Title     string         `json:"title"`
	Payload   map[string]any `json:"payload"`
	StartAt   string         `json:"start_at,omitempty"`
	EndAt     string         `json:"end_at,omitempty"`
	Status    string         `json:"status"`
	SortOrder int            `json:"sort_order"`
	Version   uint32         `json:"version"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

type CategoryDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

type ServiceabilityDTO struct {
	Serviceable bool   `json:"serviceable"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

type PublicDTO struct {
	CityCode        string            `json:"city_code"`
	Slots           []SlotDTO         `json:"slots"`
	Categories      []CategoryDTO     `json:"categories"`
	ProductBlocks   []map[string]any  `json:"product_blocks"`
	ServiceShop     any               `json:"service_shop,omitempty"`
	Serviceability  ServiceabilityDTO `json:"serviceability"`
	DeliveryPromise any               `json:"delivery_promise,omitempty"`
}
