# Go 后端 API 手工验证文档

本文档以当前 Go 路由、DTO 和鉴权实现为准，适用于本地环境的手工联调。完整的机器可读契约见 `internal/modules/docs/openapi.yaml`；服务启动后也可直接打开 `http://localhost:8080/api/v1/swagger/index.html`。

## 1. 启动与测试数据

在 `jiuxiaoer-admin/backend-go` 目录执行：

```bash
cp .env.example .env.local
make deps-up
make migrate-up
make seed
bash ./deploy/mysql/provision-local-runtime-user.sh
make run
```

`make deps-up` 在后台启动 MySQL、Redis 和 RabbitMQ，容器可能需要数秒才健康；如果紧接着执行迁移时连接失败，等待容器健康后重试。`provision-local-runtime-user.sh` 必须在迁移之后执行，它创建 `.env.local` 中 `JXE_MYSQL_DSN` 使用的低权限运行账号 `jxe_app`。迁移和 seed 使用高权限的 `JXE_MYSQL_MIGRATION_DSN`，API 使用 `JXE_MYSQL_DSN`。

默认地址为 `http://localhost:8080`，下文以此作为 `$BASE`。种子数据中的 ID 和账号如下：

| 用途 | 值 |
| --- | --- |
| 用户 | `13800000001` ，短信验证码 `123456` |
| 管理员 | `admin` / `admin123` |
| 商户 | `merchant_demo` / `merchant123` |
| 骑手 | 手机号 `13800000003`，短信验证码 `123456` |
| 示例门店 ID | `4201` |
| 示例商品 ID | `7001` ~ `7005` |
| 示例门店商品 ID | `8001` ~ `8005` |
| 示例城市与坐标 | `440300`，`22.540000,113.930000` |

管理员和商户密码来自 `.env.local` 中对应的 `JXE_*_BOOTSTRAP_PASSWORD`，如被修改请使用实际值。骑手不使用账号密码。短信 mock 默认开启，验证码固定为 `123456`。

```bash
export BASE=http://localhost:8080
curl "$BASE/livez"
curl "$BASE/readyz"
curl "$BASE/api/v1/health"
```

`/livez` 只验证进程存活；`/readyz` 会校验 MySQL、Redis、RabbitMQ 和 Snowflake 租约。后端业务 API 的统一前缀为 `/api/v1`。

## 2. 通用约定

### 鉴权

除“公开”接口外，全部携带：

```http
Authorization: Bearer <access_token>
Content-Type: application/json
```

登录成功后的 access token 位于 `data.access_token`。可按角色保存：

```bash
export CUSTOMER_TOKEN='...'
export ADMIN_TOKEN='...'
export MERCHANT_TOKEN='...'
export RIDER_TOKEN='...'
```

`$CUSTOMER_TOKEN` 等写法仅用于终端中的 curl 示例，不是后端自动创建的配置项。在 Swagger UI 中按下面操作：

1. 调用登录接口，复制响应中的纯 `data.access_token`，不要复制 JSON 引号。
2. 点击页面右上角 **Authorize**。
3. 在 `bearerAuth` 中只粘贴 Token 本身，不要手动添加 `Bearer ` 前缀。
4. 点击 **Authorize** 并关闭弹窗。Swagger 会自动发送 `Authorization: Bearer <token>`。
5. 切换顾客、管理员、商户或骑手身份时，用新角色的 Token 重新授权。

`applicationBearer` 只用于骑手申请人的受限登录态，普通顾客、管理员、商户和正式骑手接口使用 `bearerAuth`。

### 成功、分页与错误响应

普通成功响应：

```json
{
  "code": 0,
  "message": "ok",
  "data": {},
  "request_id": "req_xxx"
}
```

列表响应的 `data` 固定为：

```json
{
  "items": [],
  "next_page_token": ""
}
```

业务错误采用 Problem Details；重点关注 HTTP 状态码、`error_code` 和 `request_id`：

```json
{
  "title": "Invalid Argument",
  "status": 400,
  "detail": "...",
  "instance": "/api/v1/...",
  "error_code": "VALIDATION_FAILED",
  "request_id": "req_xxx"
}
```

### 幂等和并发版本

表格中标为“写”的业务状态变更接口要求唯一的 `Idempotency-Key`，长度为 8–128；登录、刷新和登出等认证接口除外。重复使用同一 key 且请求相同会返回首次结果；同 key 配不同请求会得到冲突错误。示例：

```bash
export IDEMPOTENCY_KEY="manual-$(date +%s)-001"
```

地址更新和首页运营位更新使用 `version` 乐观锁；先通过查询得到当前 `version`，再写入请求。过期版本返回冲突。

金额字段均为分（例如 `12900` 即 ¥129.00），所有业务 ID 都以字符串返回和传递。

### 分页

列表接口统一支持 `page_size`（1–100，默认 20）、`page_token`、`order_by`、`filter`。后两者只能使用后端白名单字段；`page_token` 必须与首次请求的其他查询条件完全一致。

## 3. 获取各角色 Token

### 顾客（首次微信登录并绑定手机号）

顾客首次必须完成“微信登录 → 绑定微信授权手机号”，之后才能使用短信登录。本地 Mock 流程要求：

```dotenv
JXE_WECHAT_AUTH_ENABLED=true
JXE_WECHAT_AUTH_MOCK_ENABLED=true
JXE_SMS_ENABLED=true
JXE_SMS_MOCK_ENABLED=true
```

修改 `.env.local` 后必须重启 `make run`，已经运行的进程不会自动重新加载配置。

#### 第一步：微信 Mock 登录

`JXE_WECHAT_AUTH_MOCK_ENABLED=true` 时，登录 `code` 必须以 `test-code-` 开头：

```bash
curl -X POST "$BASE/api/v1/auth/customer/wechat-login" \
  -H 'Content-Type: application/json' \
  -d '{"code":"test-code-manual-customer","device_id":"manual-device"}'
```

Swagger 中打开 `POST /api/v1/auth/customer/wechat-login`，点击 **Try it out**，填写相同 JSON 后执行。首次响应的 `data.phone_bound` 为 `false` 是正常结果，它只表示微信身份和顾客账号已经创建，尚未具备短信登录资格。

复制响应的 `data.access_token`。curl 用户保存为 `$CUSTOMER_TOKEN`；Swagger 用户按照“通用约定 → 鉴权”的步骤写入 `bearerAuth`。

#### 第二步：绑定微信授权手机号

Mock `phone_code` 不是短信验证码，格式必须是：

```text
test-phone-<11位中国大陆手机号>
```

例如：

```bash
curl -X POST "$BASE/api/v1/auth/customer/phone-bind" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: manual-phone-bind-001' \
  -d '{"phone_code":"test-phone-13800000010"}'
```

Swagger 中调用 `POST /api/v1/auth/customer/phone-bind`：

- 确认右上角 `bearerAuth` 已授权。
- `Idempotency-Key` 填写 8～128 位唯一值，例如 `manual-phone-bind-001`。
- 请求体填写 `{"phone_code":"test-phone-13800000010"}`，不要填写 `123456`、真实手机号本身或微信登录用的 `test-code-*`。

成功响应包含 `data.customer_id` 和 `data.phone`。如果返回 `WECHAT_PHONE_CODE_INVALID`，说明 Token 已经通过，但 `phone_code` 未被当前微信 Provider 接受；优先检查 Mock 开关、前缀、手机号长度，以及修改配置后是否重启 API。

#### 第三步：发送短信验证码

`123456` 不是始终有效的万能码。必须先调用 `send-code`，服务才会把它写入 Redis：

```bash
curl -X POST "$BASE/api/v1/auth/customer/send-code" \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800000010"}'
```

Swagger 中调用 `POST /api/v1/auth/customer/send-code`，手机号必须与第二步成功绑定的手机号完全一致。成功响应后：

- Mock 验证码固定为 `123456`。
- 有效期为 5 分钟。
- 同一手机号 60 秒内不能重复发送。
- 每个手机号每天最多发送 10 次。
- 顾客和骑手使用不同的 Redis 验证码命名空间，不能互相使用。

#### 第四步：短信登录

发送成功后再调用：

```bash
curl -X POST "$BASE/api/v1/auth/customer/sms-login" \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800000010","code":"123456"}'
```

Swagger 中调用 `POST /api/v1/auth/customer/sms-login`，填写同一个手机号和 `123456`。成功后可将新的 `data.access_token` 重新写入 `bearerAuth`。

验证码校验成功时会立即从 Redis 删除，因此只能使用一次。当前实现先消费验证码，再检查顾客是否已经完成微信登录和手机号绑定；如果返回 `403 AUTH_WECHAT_LOGIN_REQUIRED`，这枚正确验证码也已经失效。完成绑定后必须重新调用 `send-code`，再使用新的 `123456` 登录。

`send-code` 只发送验证码，不注册顾客账号。真实环境关闭 Mock 后，后端生成随机六位验证码并通过腾讯云发送。

#### 顾客登录常见错误

| 错误码 | 含义 | 处理 |
| --- | --- | --- |
| `WECHAT_CODE_INVALID` | 微信 Mock 登录 code 无效 | 使用 `test-code-` 开头的 code，确认微信认证 Mock 已开启 |
| `WECHAT_PHONE_CODE_INVALID` | 微信手机号授权 code 无效 | 使用 `test-phone-` 加 11 位手机号，确认 Mock 开关并重启 API |
| `AUTH_INVALID_CODE` | Redis 中没有匹配的短信验证码 | 重新调用同一作用域的 `send-code`，在 5 分钟内使用一次 |
| `AUTH_WECHAT_LOGIN_REQUIRED` | 手机号未绑定到当前小程序微信身份 | 先完成微信登录和 phone-bind，然后重新发送验证码 |
| `PHONE_ALREADY_BOUND` | 手机号已属于其他顾客 | 更换手机号，或使用原顾客身份登录 |
| `AUTH_SMS_TOO_FREQUENT` | 60 秒内重复发送 | 等待冷却时间结束 |
| `AUTH_SMS_DAILY_LIMIT` | 当天发送超过 10 次 | 更换测试手机号或等待限额过期 |
| `AUTH_LOGIN_RATE_LIMITED` | 15 分钟内错误登录次数达到限制 | 停止重复尝试，等待限制过期后重新发送验证码 |
| `SYSTEM_DEPENDENCY_UNAVAILABLE` | Redis 或短信 Provider 不可用 | 检查 `make deps-up`、Redis 健康和短信配置 |

### 管理员、商户和骑手

```bash
curl -X POST "$BASE/api/v1/auth/admin/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}'

curl -X POST "$BASE/api/v1/auth/merchant/login" -H 'Content-Type: application/json' \
  -d '{"username":"merchant_demo","password":"merchant123"}'

curl -X POST "$BASE/api/v1/auth/rider/send-code" -H 'Content-Type: application/json' \
  -d '{"phone":"13800000003"}'

curl -X POST "$BASE/api/v1/auth/rider/sms-login" -H 'Content-Type: application/json' \
  -d '{"phone":"13800000003","code":"123456"}'
```

骑手正式登录只接受手机号和短信验证码，旧的 `/auth/rider/login` 账号密码入口不存在。骑手也必须先调用 `/auth/rider/send-code`，才能使用 Mock 验证码 `123456`。顾客和骑手共用短信 Provider，但验证码使用独立 Redis 命名空间；验证码有效期 5 分钟、校验成功后立即失效。

### 生产腾讯云短信配置

生产环境必须关闭 Mock，并配置腾讯云短信 API 3.0：

```dotenv
JXE_SMS_ENABLED=true
JXE_SMS_MOCK_ENABLED=false
JXE_SMS_PROVIDER=tencentcloud
JXE_SMS_TENCENTCLOUD_REGION=ap-guangzhou
JXE_SMS_TENCENTCLOUD_SECRET_ID=<secret-manager 注入>
JXE_SMS_TENCENTCLOUD_SECRET_KEY=<secret-manager 注入>
JXE_SMS_TENCENTCLOUD_SDK_APP_ID=<短信应用 SDK AppID>
JXE_SMS_TENCENTCLOUD_SIGN_NAME=<已审核签名内容>
JXE_SMS_TENCENTCLOUD_TEMPLATE_ID=<已审核验证码模板 ID>
JXE_SMS_TENCENTCLOUD_ENDPOINT=sms.tencentcloudapi.com
JXE_SMS_HTTP_TIMEOUT=5s
```

验证码模板必须包含两个变量，顺序为“验证码、有效分钟数”，例如：`您的验证码为{1}，{2}分钟内有效。`。SecretID 和 SecretKey 只能通过部署密钥管理器注入，不能提交到仓库。

刷新、登出：

```bash
curl -X POST "$BASE/api/v1/auth/refresh" -H 'Content-Type: application/json' \
  -d '{"refresh_token":"<refresh_token>"}'

curl -X POST "$BASE/api/v1/auth/logout" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN"
```

## 4. 接口清单

“公开”表示无需 Bearer Token；其余接口需要表中角色对应的 Token。标为“写”的接口需 `Idempotency-Key`。

### 健康、文档与公开目录

| 方法 | 路径 | 权限 | 主要参数 / 说明 |
| --- | --- | --- | --- |
| GET | `/livez` | 公开 | 进程存活（不带 API 信封） |
| GET | `/readyz` | 公开 | 依赖就绪（不带 API 信封） |
| GET | `/metrics` | 公开/配置 | Prometheus；生产环境需 `Authorization: Bearer $JXE_METRICS_TOKEN` |
| GET | `/api/v1/health` | 公开 | API 信封的就绪检查 |
| GET | `/api/v1/docs/openapi.yaml` | 公开 | OpenAPI 3.0 文档 |
| GET | `/api/v1/swagger` | 公开 | 重定向至 Swagger UI |
| GET | `/api/v1/swagger/index.html` | 公开 | Swagger UI |
| GET | `/api/v1/categories` | 公开 | 分类列表 |
| GET | `/api/v1/shops` | 公开 | `city`、`district`、`keyword`、`city_code`、`lat`、`lng`、分页 |
| GET | `/api/v1/products` | 公开 | `shop_id`、`category_id`、`keyword`、`city_code`、`lat`、`lng`、分页 |
| GET | `/api/v1/products/{id}` | 公开 | 可选 `shop_id`、`city_code`、`lat`、`lng` |
| GET | `/api/v1/service-shops/resolve` | 公开 | 必填 `city_code`、`lat`、`lng`；解析唯一服务门店 |
| GET | `/api/v1/home` | 公开 | `city_code`；`lat`、`lng` 必须同时提供或同时省略 |

位置坐标成对校验；经纬度不完整时返回 `LOCATION_REQUIRED`，无效坐标返回 `VALIDATION_INVALID_QUERY`。服务区解析必须提供合法城市码和经纬度。

### 顾客：购物车与地址

| 方法 | 路径 | 权限 | 请求体 / 参数 |
| --- | --- | --- | --- |
| GET | `/api/v1/cart/items` | 顾客 | 返回整个购物车汇总 |
| POST | `/api/v1/cart/items` | 顾客，写 | `{"shop_product_id":"8001","quantity":1}` |
| PUT | `/api/v1/cart/items/{id}` | 顾客，写 | `{"quantity":2}` |
| PATCH | `/api/v1/cart/items/{id}/selection` | 顾客，写 | `{"selected":true}` |
| POST | `/api/v1/cart/selection` | 顾客，写 | `{"shop_id":"4201","selected":true}` |
| DELETE | `/api/v1/cart/items/{id}` | 顾客，写 | 删除一项 |
| DELETE | `/api/v1/cart/items?shop_id=4201` | 顾客，写 | `shop_id` 可省略；省略时清空全部 |
| GET | `/api/v1/addresses` | 顾客 | 地址列表 |
| POST | `/api/v1/addresses` | 顾客，写 | 见下方地址示例 |
| PUT | `/api/v1/addresses/{id}` | 顾客，写 | 完整地址字段 + 必填 `version` |
| DELETE | `/api/v1/addresses/{id}` | 顾客，写 | 软删除 |
| POST | `/api/v1/addresses/{id}/set-default` | 顾客，写 | 设置默认地址 |

创建地址示例：

```bash
curl -X POST "$BASE/api/v1/addresses" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
  -d '{
    "contact_name":"测试用户", "contact_phone":"13800000010",
    "province":"广东省", "city":"深圳市", "city_code":"440300",
    "district":"南山区", "district_code":"440305",
    "address_detail":"科技园测试路 1 号", "doorplate":"101",
    "latitude":22.540000, "longitude":113.930000, "is_default":true
  }'
```

记录响应的 `data.id` 为 `$ADDRESS_ID`，`data.version` 用于后续更新。

### 顾客：订单与支付

| 方法 | 路径 | 权限 | 请求体 / 说明 |
| --- | --- | --- | --- |
| GET | `/api/v1/orders` | 顾客 | 本人订单分页列表 |
| POST | `/api/v1/orders` | 顾客，写 | `shop_id`、`address_id`、`items` 必填 |
| GET | `/api/v1/orders/{id}` | 顾客 | 本人订单详情 |
| POST | `/api/v1/orders/{id}/cancel` | 顾客或有 `order:cancel` 的管理员，写 | `{"reason":"不需要了"}`；仅待支付订单可取消 |
| POST | `/api/v1/orders/{id}/payments` | 顾客，写 | 微信支付单：`{"provider":"wechat","client_type":"miniapp","return_context":{}}`；要求有效小程序身份且已绑定手机号 |
| GET | `/api/v1/orders/{id}/payment` | 顾客 | 查询持久化支付状态 |
| POST | `/api/v1/orders/{id}/pay/mock` | 顾客，写 | `{"channel":"mock"}`；仅 `JXE_PAYMENT_MOCK_ENABLED=true` 时注册 |
| POST | `/api/v1/payments/{provider}/callbacks` | 支付供应商 | 无 JWT；仅微信支付开启时注册，需供应商签名头 |

创建订单示例（先创建地址）：

```bash
curl -X POST "$BASE/api/v1/orders" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: manual-order-001" \
  -d '{
    "shop_id":"4201", "address_id":"'$ADDRESS_ID'",
    "items":[{"shop_product_id":"8001","quantity":1}],
    "remark":"手工验证订单"
  }'
```

创建成功后保存 `data.order_id` 为 `$ORDER_ID`。本地推荐调用 mock 支付完成支付状态迁移：

```bash
curl -X POST "$BASE/api/v1/orders/$ORDER_ID/pay/mock" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: manual-pay-001' \
  -d '{"channel":"mock"}'
```

真实微信支付接口需有效的当前小程序身份、已绑定手机号及支付配置。已有微信身份但未绑定手机号时，创建支付返回 `409 PHONE_BINDING_REQUIRED`，不创建支付记录，也不调用微信支付。回调不是可随机构造的 JSON 接口，必须使用微信支付签名后的请求。回调成功响应为 `{"code":"SUCCESS","message":"成功"}`。

### 顾客：实名认证与成年校验

| 方法 | 路径 | 权限 | 请求体 / 说明 |
| --- | --- | --- | --- |
| GET | `/api/v1/identity-verifications/me` | 顾客 | 当前实名成年授权事实 |
| POST | `/api/v1/identity-verifications` | 顾客，写 | 创建服务商托管会话；`verification_level`、`consent_version` 必填 |
| GET | `/api/v1/identity-verifications/{id}` | 顾客 | 仅可轮询本人单次认证请求 |
| POST | `/api/v1/identity-verifications/{provider}/callbacks` | 认证服务商 | 无 JWT；必须携带服务商签名，后端随后主动查询结果 |

```bash
curl -X POST "$BASE/api/v1/identity-verifications" \
  -H "Authorization: Bearer $CUSTOMER_TOKEN" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: manual-identity-001' \
  -d '{
    "purpose":"alcohol_purchase",
    "verification_level":"identity_and_liveness",
    "consent_version":"privacy-2026-07"
  }'
```

接口返回 `202`、`data.verification_id`、`data.session_url` 和
`data.session_expires_at`。客户端跳转到 `session_url` 完成证件/人脸流程，
然后按 `verification_id` 轮询；客户端不得向本系统提交姓名、证件号、证件图
或人脸图。真实回调只能由服务商发起，前端不应直接模拟生产回调。

受限商品订单的判定为：`verified + adult + 未撤销 + 未超过可选 valid_until`
才允许；处理中返回 `IDENTITY_VERIFICATION_PENDING`，未实名/未知/过期/撤销
返回 `REALNAME_REQUIRED`，未成年返回 `UNDERAGE_RESTRICTED`。成年结果默认长期
有效，不会自动设置一年有效期。

### 商户端

| 方法 | 路径 | 权限 | 请求体 / 说明 |
| --- | --- | --- | --- |
| GET | `/api/v1/store/orders` | 商户 | 可按 `status` 筛选，且只返回被授权门店订单 |
| POST | `/api/v1/store/orders/{id}/accept` | 商户，写 | 接单 |
| POST | `/api/v1/store/orders/{id}/start-preparing` | 商户，写 | 开始备货 |
| POST | `/api/v1/store/orders/{id}/prepare` | 商户，写 | 备货完成 |
| PATCH | `/api/v1/store/shops/{id}/business-status` | 商户，写 | `{"business_status":"open"}`；可用 `open`、`closed`、`resting` |
| GET | `/api/v1/store/shop-products` | 商户 | 可选 `shop_id`，仅被授权门店 |
| POST | `/api/v1/store/shop-products` | 商户，写 | `shop_id`、`product_id`、`sale_price_amount` 必填 |
| PATCH | `/api/v1/store/shop-products/{id}` | 商户，写 | 可更新 `sale_price_amount`、`status`、`sort_order` |
| PATCH | `/api/v1/store/shop-products/{id}/stock` | 商户，写 | `{"quantity_delta":10,"reason":"入库"}` |

商户履约顺序：已支付订单 → `accept` → `start-preparing` → `prepare`。每一步成功后均检查订单状态字段是否按预期迁移。

### 骑手端

| 方法 | 路径 | 权限 | 请求体 / 说明 |
| --- | --- | --- | --- |
| POST | `/api/v1/auth/rider/send-code` | 公开 | `{"phone":"13800000003"}`；申请和登录共用骑手验证码 |
| POST | `/api/v1/rider-applications` | 公开，写 | `name`、`phone`、`code`、`service_scope.shop_ids`；创建待审核申请 |
| POST | `/api/v1/auth/rider-application/sms-login` | 公开 | 待审核申请人使用 `phone`、`code` 获取受限 token |
| POST | `/api/v1/auth/rider/sms-login` | 公开 | 仅审核通过且已启用的骑手可登录 |
| GET | `/api/v1/delivery/orders` | 骑手 | 可按 `status` 筛选；返回可接和本人配送单 |
| POST | `/api/v1/delivery/orders/{id}/accept` | 骑手，写 | 接配送单 |
| POST | `/api/v1/delivery/orders/{id}/pickup` | 骑手，写 | 取货并开始配送 |
| POST | `/api/v1/delivery/orders/{id}/complete` | 骑手，写 | 完成配送 |

骑手申请会先按手机号、路径、幂等键和请求摘要检查已完成结果，再消费一次性短信验证码。相同幂等键与相同请求重试会返回首次申请；使用新幂等键提交、申请人登录或正式骑手登录时，都必须先重新发送并使用一枚未消费的验证码。

骑手履约顺序：商户备货完成 → `accept` → `pickup` → `complete`。`pickup` 会直接将配送单转为配送中。这里的 `{id}` 为配送单 ID，不是订单 ID；先通过配送单列表取得 `data.items[].id`。

### 管理端

管理端需管理员身份并校验权限码；种子 `admin` 是超级管理员，具备全部权限。

| 方法 | 路径 | 权限 | 请求体 / 说明 |
| --- | --- | --- | --- |
| POST | `/api/v1/admin/riders` | `rider:create`，写 | 管理员创建待审核骑手；只提交 `name`、`phone`、`service_scope.shop_ids` |
| POST | `/api/v1/admin/riders/{id}/review` | `rider:review`，写 | `{"decision":"approved","reason":"资料核验通过"}`；批准后账号才激活 |
| POST | `/api/v1/admin/products` | `product:create`，写 | 创建平台商品 |
| PUT | `/api/v1/admin/products/{id}` | `product:update`，写 | 可部分更新商品字段 |
| POST | `/api/v1/admin/products/{id}/on-sale` | `product:update`，写 | 上架 |
| POST | `/api/v1/admin/products/{id}/off-sale` | `product:update`，写 | 下架 |
| GET | `/api/v1/admin/orders` | `order:list` | 可按 `status` 筛选 |
| GET | `/api/v1/admin/orders/{id}` | `order:view` | 订单详情 |
| GET | `/api/v1/admin/stocks` | `inventory:view` | 可选 `shop_id` |
| POST | `/api/v1/admin/stocks/adjust` | `inventory:adjust`，写 | `{"shop_product_id":"8001","quantity_delta":10,"reason":"盘点"}` |
| GET | `/api/v1/admin/audit-logs` | `audit_log:view` | 审计日志分页列表 |
| GET | `/api/v1/admin/merchants` | `merchant:list` | 可按 `review_status` 筛选 |
| POST | `/api/v1/admin/merchants/{id}/review` | `merchant:review`，写 | `{"review_status":"approved","review_remark":""}` 或 `rejected` |
| GET | `/api/v1/admin/home-slots` | `home_slot:list` | `city_code`、`slot_type`、`status`、分页 |
| POST | `/api/v1/admin/home-slots` | `home_slot:create`，写 | 见首页运营位示例 |
| PUT | `/api/v1/admin/home-slots/{id}` | `home_slot:update`，写 | 完整运营位字段 + `version` |
| POST | `/api/v1/admin/home-slots/{id}/status` | `home_slot:publish`，写 | `{"status":"published","version":1}` |

创建首页运营位示例：

```bash
curl -X POST "$BASE/api/v1/admin/home-slots" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: manual-slot-001' \
  -d '{
    "city_code":"440300", "slot_type":"product_block", "slot_key":"manual-test",
    "title":"手工验证运营位", "payload":{"product_ids":["7001"]},
    "sort_order":1
  }'
```

再将返回的 ID 和 version 用于发布。发布时会验证 `payload.product_ids` / `payload.category_ids` 中引用的商品或分类是否存在。

## 5. 推荐端到端验收顺序

1. 执行健康检查，确认 `/readyz` 为 `ok`。
2. 顾客首次通过当前微信小程序登录并绑定手机号，获取 `$CUSTOMER_TOKEN`；后续可选择微信或短信登录。管理员和商户用账号密码登录，骑手用手机号短信登录。
3. 查询 `/shops?city_code=440300&lat=22.54&lng=113.93`、`/products?shop_id=4201`，确认种子门店商品存在。
4. 创建地址，记录 `$ADDRESS_ID`；创建订单，记录 `$ORDER_ID`。
5. 使用 `/orders/{id}/pay/mock` 完成支付，读取 `/orders/{id}/payment` 确认 `status=succeeded`，读取订单确认 `status=paid`。
6. 用商户 token 按 `accept → start-preparing → prepare` 处理订单。
7. 用骑手 token 在 `/delivery/orders` 找到配送单 ID，按 `accept → pickup → complete` 完成配送。
8. 用顾客 token 查询订单，用管理员 token 查询订单、库存和审计日志，确认最终状态、库存扣减和操作记录。

建议对每个写接口额外验证一次幂等性：完全相同的请求与 `Idempotency-Key` 重发，应返回首次结果且不产生重复订单、重复库存变动或重复状态迁移。

## 6. 常见预期失败用例

| 场景 | 预期 |
| --- | --- |
| 无或错误 Bearer Token | `401 AUTH_UNAUTHORIZED` |
| 角色不匹配，例如顾客调用商户接口 | `403 PERM_FORBIDDEN` |
| 缺少/复用错误的幂等 key | 幂等校验错误或 `409 IDEMPOTENCY_CONFLICT` |
| 数量小于 1 或大于 99 | `400 VALIDATION_FAILED` 或订单参数错误 |
| 地址版本过期 | `409` 版本冲突 |
| Swagger 的 `bearerAuth` 中粘贴了 `Bearer <token>` | 可能形成重复前缀并返回 `401 AUTH_UNAUTHORIZED`；只粘贴纯 Token |
| Mock `phone_code` 填写为 `123456` 或裸手机号 | `401 WECHAT_PHONE_CODE_INVALID` |
| 未调用 `send-code` 就直接使用 `123456` | `401 AUTH_INVALID_CODE` |
| 短信验证码已使用一次或超过 5 分钟 | `401 AUTH_INVALID_CODE` |
| C 端未完成当前小程序微信登录和手机号绑定就调用短信登录 | `403 AUTH_WECHAT_LOGIN_REQUIRED`，且不创建账号 |
| 已微信登录但未绑定手机号就发起真实微信支付 | `409 PHONE_BINDING_REQUIRED`，不创建支付记录且不调用支付提供器 |
| 已支付订单取消 | `409 ORDER_INVALID_STATUS` |
| 已完成状态再次履约 | `409` 状态迁移冲突 |
| 库存不足 | `409 STOCK_NOT_ENOUGH` 或库存相关冲突 |
| 位置参数只传经度/纬度之一 | `422 LOCATION_REQUIRED` |
| mock 功能关闭后访问 mock 路由 | 路由不存在（404） |

如需精确字段类型、全部响应模型或直接在页面中发送请求，请以 Swagger UI 和 `openapi.yaml` 为准；本手册聚焦可重复执行的人工验收路径。
