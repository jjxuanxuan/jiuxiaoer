package servicearea

type ResolvedShop struct {
	ID                          uint64
	MerchantID                  uint64
	Name                        string
	CityCode                    string
	District                    string
	Address                     string
	Latitude                    float64
	Longitude                   float64
	DistanceM                   int64
	ServiceAreaVersion          uint32
	Priority                    int
	DeliveryFeeAmount           int64
	FreeDeliveryThresholdAmount *int64
	DeliveryETAMin              uint16
	DeliveryETAMax              uint16
	OvertimePolicyCode          *string
	OvertimePolicyVersion       *uint32
	PolicyTitle                 *string
	PolicySummary               *string
	PolicyTermsURL              *string
}

// DeliveryFee 返回配送 Fee。
func (s ResolvedShop) DeliveryFee(goodsAmount int64) int64 {
	if s.FreeDeliveryThresholdAmount != nil && goodsAmount >= *s.FreeDeliveryThresholdAmount {
		return 0
	}
	return s.DeliveryFeeAmount
}
