package reminder

import "jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"

const RedemptionAllocationStatusHeld = allocationStatusHeld
const LotSourcePurchase = core.LotSourcePurchase
const LotStatusActive = core.LotStatusActive
const LotStatusDepleted = core.LotStatusDepleted

var shanghaiLocation = core.ShanghaiLocation

func idString(value uint64) string { return core.IDString(value) }
