package shop

type Shop struct {
	ID                          uint64
	MerchantID                  uint64
	Name                        string
	Phone                       *string
	City                        string
	CityCode                    *string
	District                    string
	Address                     string
	Latitude                    *float64
	Longitude                   *float64
	CoordinateSystem            string
	Status                      string
	BusinessStatus              string
	ServiceRadiusM              int64
	Priority                    int
	DeliveryFeeAmount           int64
	FreeDeliveryThresholdAmount *int64
	DeliveryETAMin              uint16
	DeliveryETAMax              uint16
	OvertimePolicyCode          *string
	DistanceM                   *int64
	Serviceable                 *bool
}

// TableName 返回当前数据模型对应的数据库表名。
func (Shop) TableName() string { return "shops" }
