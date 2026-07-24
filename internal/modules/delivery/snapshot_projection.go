package delivery

import "math"

// 快照投影刻意只复制文档规定的履约字段。已存储 JSON 可能包含内部验证、
// 服务商或身份凭证，绝不能完整返回给骑手客户端。
func deliveryPickupSnapshotDTO(snapshot map[string]any) *DeliveryPickupSnapshotDTO {
	if len(snapshot) == 0 {
		return nil
	}
	return &DeliveryPickupSnapshotDTO{
		ShopID: snapshotString(snapshot, "shop_id"), Name: snapshotString(snapshot, "name", "shop_name"),
		Phone: snapshotString(snapshot, "phone", "contact_phone"), District: snapshotString(snapshot, "district"),
		Address: snapshotString(snapshot, "address", "address_detail"), Latitude: snapshotCoordinate(snapshot, "latitude", -90, 90),
		Longitude: snapshotCoordinate(snapshot, "longitude", -180, 180), CoordinateSystem: snapshotString(snapshot, "coordinate_system"),
	}
}

func deliveryRecipientSnapshotDTO(snapshot map[string]any) *DeliveryRecipientSnapshotDTO {
	if len(snapshot) == 0 {
		return nil
	}
	return &DeliveryRecipientSnapshotDTO{
		ContactName: snapshotString(snapshot, "contact_name"), ContactPhone: snapshotString(snapshot, "contact_phone"),
		Province: snapshotString(snapshot, "province"), City: snapshotString(snapshot, "city"), CityCode: snapshotString(snapshot, "city_code"),
		District: snapshotString(snapshot, "district"), DistrictCode: snapshotString(snapshot, "district_code"),
		AddressDetail: snapshotString(snapshot, "address_detail"), Doorplate: snapshotString(snapshot, "doorplate"),
		POIID: snapshotString(snapshot, "poi_id"), FormattedAddress: snapshotString(snapshot, "formatted_address"),
		Latitude: snapshotCoordinate(snapshot, "latitude", -90, 90), Longitude: snapshotCoordinate(snapshot, "longitude", -180, 180),
		CoordinateSystem: snapshotString(snapshot, "coordinate_system"), LocationSource: snapshotString(snapshot, "location_source"),
		GeocodeProvider: snapshotString(snapshot, "geocode_provider"), GeocodeStatus: snapshotString(snapshot, "geocode_status"),
		AddressVersion: snapshotUint32(snapshot, "address_version"),
	}
}

func deliveryProductSnapshotDTO(snapshot map[string]any) DeliveryProductSnapshotDTO {
	policy, _ := snapshot["return_policy"].(map[string]any)
	return DeliveryProductSnapshotDTO{
		Name: snapshotString(snapshot, "name"), BrandName: snapshotString(snapshot, "brand_name"),
		Spec: snapshotString(snapshot, "spec"), ImageURL: snapshotString(snapshot, "image_url"),
		AgeRestricted: snapshotBool(snapshot, "age_restricted"),
		ReturnPolicy: DeliveryReturnPolicySnapshotDTO{
			Eligible: snapshotBool(policy, "eligible"), PolicyCode: snapshotString(policy, "policy_code"),
			PolicyVersion: snapshotString(policy, "policy_version"), SealedPackageRequired: snapshotBool(policy, "sealed_package_required"),
		},
	}
}

func snapshotCoordinate(snapshot map[string]any, key string, minimum, maximum float64) *float64 {
	value, ok := snapshot[key].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < minimum || value > maximum {
		return nil
	}
	return &value
}

func snapshotUint32(snapshot map[string]any, key string) uint32 {
	value, ok := snapshot[key].(float64)
	if !ok || value < 0 || value > math.MaxUint32 || math.Trunc(value) != value {
		return 0
	}
	return uint32(value)
}

func snapshotBool(snapshot map[string]any, key string) bool {
	value, _ := snapshot[key].(bool)
	return value
}
