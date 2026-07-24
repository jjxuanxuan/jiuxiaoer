package printjob

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type receiptOrderRow struct {
	ID                uint64
	OrderNo           string
	ShopID            uint64
	GoodsAmount       int64
	DiscountAmount    int64
	DeliveryFeeAmount int64
	PayableAmount     int64
	PaidAmount        int64
	Remark            *string
	AddressSnapshot   datatypes.JSON
	PaidAt            *time.Time
	CreatedAt         time.Time
	ShopName          string
	ShopAddress       string
	ShopPhone         *string
}

type receiptItemRow struct {
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
	SalePriceAmount int64
	TotalAmount     int64
}

// renderReceiptV1 构建不可变且与服务商无关的快照。
// 其设计会排除精确坐标、验证码、完整手机号及其他履约秘密。
func renderReceiptV1(tx *gorm.DB, orderID, shopID uint64, template Template) (datatypes.JSON, error) {
	var order receiptOrderRow
	err := tx.Table("orders o").
		Select("o.id,o.order_no,o.shop_id,o.goods_amount,o.discount_amount,o.delivery_fee_amount,o.payable_amount,o.paid_amount,o.remark,o.address_snapshot,o.paid_at,o.created_at,s.name AS shop_name,s.address AS shop_address,s.phone AS shop_phone").
		Joins("JOIN shops s ON s.id=o.shop_id AND s.deleted_at IS NULL").
		Where("o.id=? AND o.shop_id=? AND o.deleted_at IS NULL", orderID, shopID).
		Take(&order).Error
	if err != nil {
		return nil, err
	}
	var rows []receiptItemRow
	if err := tx.Table("order_items").
		Select("shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount").
		Where("order_id=? AND deleted_at IS NULL", orderID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("receipt order has no items")
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var snapshot map[string]any
		_ = json.Unmarshal(row.ProductSnapshot, &snapshot)
		name := textValue(snapshot["name"])
		if name == "" {
			name = "商品"
		}
		items = append(items, map[string]any{
			"product_id": idString(row.ProductID), "shop_product_id": idString(row.ShopProductID),
			"name": name, "brand_name": textValue(snapshot["brand_name"]), "spec": textValue(snapshot["spec"]),
			"unit_price_amount": row.SalePriceAmount, "quantity": row.Quantity, "total_amount": row.TotalAmount,
		})
	}
	var address map[string]any
	_ = json.Unmarshal(order.AddressSnapshot, &address)
	formattedAddress := textValue(address["formatted_address"])
	if formattedAddress == "" {
		formattedAddress = strings.Join(nonEmptyStrings(
			textValue(address["province"]), textValue(address["city"]), textValue(address["district"]),
			textValue(address["address_detail"]), textValue(address["doorplate"]),
		), "")
	}
	paidAt := any(nil)
	if order.PaidAt != nil {
		paidAt = order.PaidAt.UTC().Format(time.RFC3339)
	}
	payload := map[string]any{
		"schema_version": "receipt.v1", "template_version": template.Version,
		"order": map[string]any{
			"order_id": idString(order.ID), "order_no": order.OrderNo,
			"created_at": order.CreatedAt.UTC().Format(time.RFC3339), "paid_at": paidAt, "remark": order.Remark,
		},
		"shop": map[string]any{
			"shop_id": idString(order.ShopID), "name": order.ShopName, "address": order.ShopAddress,
			"phone_mask": maskPhone(pointerString(order.ShopPhone)),
		},
		"items": items,
		"amounts": map[string]any{
			"goods": order.GoodsAmount, "discount": order.DiscountAmount, "delivery_fee": order.DeliveryFeeAmount,
			"payable": order.PayableAmount, "paid": order.PaidAmount, "currency": "CNY",
		},
		"recipient": map[string]any{
			"name_mask":         maskName(textValue(address["contact_name"])),
			"phone_mask":        maskPhone(textValue(address["contact_phone"])),
			"formatted_address": formattedAddress,
		},
		"fulfillment":  map[string]any{"delivery_mode": "delivery"},
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	return datatypes.JSON(raw), err
}

// RenderReceiptV1ForBackfill 向受控 CP1 迁移命令公开同一不可变小票投影。
// 让迁移渲染继续使用此路径，可防止一次性脚本与新建任务产生偏差。
// 调用方仍负责只写入可迁移任务状态并应用乐观更新。
func RenderReceiptV1ForBackfill(tx *gorm.DB, orderID, shopID uint64, template Template) (datatypes.JSON, error) {
	return renderReceiptV1(tx, orderID, shopID, template)
}

func textValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func maskName(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return "*"
	}
	return string(runes[0]) + "**"
}

func maskPhone(value string) string {
	digits := make([]rune, 0, len(value))
	for _, char := range value {
		if char >= '0' && char <= '9' {
			digits = append(digits, char)
		}
	}
	if len(digits) >= 7 {
		return string(digits[:3]) + "****" + string(digits[len(digits)-4:])
	}
	if len(digits) > 0 {
		return strings.Repeat("*", len(digits))
	}
	return "*******"
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
