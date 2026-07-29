# 九小二 Go 后端

九小二的 Go API 服务，覆盖顾客、商户、骑手和平台管理员四类角色。项目采用模块化单体结构，对外接口以 OpenAPI/Swagger 为准，适合本地启动后直接进行前端联调。

## 功能范围

- 顾客端：微信/短信登录、首页、搜索、商品、门店、购物车、地址、下单、支付、会员与资产。
- 商户端：门店和商品管理、接单备货、售后处理、配送协作、打印任务。
- 骑手端：短信登录、接单、配送状态、定位、路线、配送异常与退回。
- 平台端：管理员与权限、商户开通、订单运营、售后退款、会员资产、调度、消息和审计。

部分高风险或依赖第三方服务的功能由环境变量控制，路由存在不代表功能默认开启。

## 技术栈

- Go 1.26、Gin、GORM
- MySQL 8.4、Redis 8、RabbitMQ 4
- Goose 数据库迁移
- JWT 鉴权
- OpenAPI 3 与 Swagger UI

## 本地启动

### 环境要求

- Go 1.26 或更高版本
- Docker Desktop（含 Docker Compose）
- Make

首次执行 Goose 或 `go run` 时，Go 可能需要联网下载依赖。

### 1. 准备配置

```bash
cp .env.example .env.local
```

`.env.example` 是可启动本地环境的配置模板；`.env.local` 是个人运行配置，已被 Git 忽略，不应提交或作为交付文件传递。

### 2. 启动依赖并初始化数据库

```bash
make deps-up
make migrate-up
make seed
bash ./deploy/mysql/provision-local-runtime-user.sh
```

`make deps-up` 会在后台启动 MySQL、Redis 和 RabbitMQ。容器首次启动可能需要数秒；如果迁移提示连接失败，等待容器健康后重试：

```bash
docker compose -f deploy/docker-compose.local.yml --profile mq ps
```

初始化步骤的职责如下：

- `make migrate-up`：按版本顺序创建或升级数据库结构。
- `make seed`：写入管理员、商户、骑手、门店和商品等本地演示数据。
- `provision-local-runtime-user.sh`：为 API 创建低权限 MySQL 运行账号。新增迁移后应重新执行此脚本，以补充新表权限。

### 3. 启动 API

```bash
make run
```

默认监听 `http://localhost:8080`。`make run` 除 HTTP API 外，还会启动当前配置已启用的进程内任务，前端本地联调通常不需要再执行 `make run-worker`。

### 4. 验证服务

| 地址 | 用途 |
| --- | --- |
| `http://localhost:8080/livez` | 进程存活检查 |
| `http://localhost:8080/readyz` | MySQL、Redis、RabbitMQ 和节点租约就绪检查 |
| `http://localhost:8080/api/v1/health` | API 格式的健康检查 |
| `http://localhost:8080/api/v1/swagger/index.html` | Swagger UI |
| `http://localhost:8080/metrics` | Prometheus 指标 |

## 前端联调

业务 API 的统一前缀是 `/api/v1`。建议先通过 Swagger 完成登录并获取 Token，再调试业务接口。

### 本地演示账号

| 角色 | 登录信息 |
| --- | --- |
| 用户 | `13800000001` ，短信验证码 `123456` |
| 管理员 | `admin` / `admin123` |
| 商户 | `merchant_demo` / `merchant123` |
| 骑手 | 手机号 `13800000003`，先发送验证码，再使用 Mock 验证码 `123456` |
| 顾客 | 首次使用微信 Mock 登录并绑定手机号；之后才可短信登录 |

管理员和商户密码来自 `.env.local` 中对应的 `JXE_*_BOOTSTRAP_PASSWORD`。如果修改过配置，应使用修改后的密码并重新执行 `make seed`。

顾客本地 Mock 流程：

1. 调用 `/auth/customer/wechat-login`，`code` 使用 `test-code-` 前缀。
2. 将返回的 `data.access_token` 填入 Swagger 的 `bearerAuth`；只粘贴 Token，不要手动添加 `Bearer `。
3. 调用 `/auth/customer/phone-bind`，`phone_code` 使用 `test-phone-<11位手机号>`。
4. 后续短信登录必须先调用 `/auth/customer/send-code`，再使用验证码 `123456`。

骑手短信登录同样必须先调用 `/auth/rider/send-code`。Mock 验证码不是永久万能码：它有有效期，验证成功后只能使用一次。

接口通用约定：

- 业务 ID 在 JSON 中按字符串传递。
- 金额单位为分，例如 `12900` 表示 ¥129.00。
- 需要幂等保护的写接口必须携带唯一的 `Idempotency-Key`。
- 业务错误采用 Problem Details，前端应重点读取 HTTP 状态码、`error_code` 和 `request_id`。

完整登录流程、请求示例、接口清单和常见错误见 [API 手工验证文档](docs/runbooks/api-manual-test.md)。

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `make run` | 加载 `.env.local` 并启动 API |
| `make run-worker` | 单独启动指定 MQ/后台任务进程；`JXE_WORKER_ROLE` 未设置时使用 `all` |
| `make test` | 执行 `go test ./...` |
| `make tidy` | 整理依赖，会更新 `go.mod` 和 `go.sum` |
| `make deps-up` | 启动本地 MySQL、Redis 和 RabbitMQ |
| `make deps-mq-up` | 只启动 RabbitMQ |
| `make deps-down` | 停止本地依赖，保留 Docker 数据卷 |
| `make migrate-up` | 执行尚未应用的数据库迁移 |
| `make migrate-down` | 回滚最近一条迁移，会修改数据库，请谨慎使用 |
| `make seed` | 写入或更新本地演示数据 |

如需把 Worker 拆成独立进程，应为每个进程设置不同的 `JXE_INSTANCE_ID` 和 `JXE_SNOWFLAKE_NODE_ID`，避免节点租约冲突。
酒票数据库后台闭环可使用
`JXE_WINE_TICKET_MAINTENANCE_OWNER=worker JXE_WORKER_ROLE=wine-ticket-maintenance /app/jiuxiaoer-worker`
独立运行；默认 `JXE_WINE_TICKET_MAINTENANCE_OWNER=api`，保持 API
进程内任务的兼容行为。独立角色会拒绝 owner 不是 `worker` 的配置。
该角色不依赖 RabbitMQ，负责支付收敛、转赠超时、T-7/到期、退款执行和酒票对账。
由于支付过期和退款执行是普通零售与酒票共用的任务队列，owner 切到
`worker` 时这两类共享任务也整体转移到该进程；API 不再启动它们，保证
普通零售与酒票都不会漏扫或双跑。同一环境的所有 API 和该 Worker 必须
使用相同 owner 值。

## 项目结构

```text
.
├── cmd/
│   ├── api/                 # HTTP API 入口
│   ├── worker/              # 独立 Worker 入口
│   ├── seed/                # 本地演示数据
│   └── mq-topology/         # RabbitMQ 拓扑工具
├── internal/
│   ├── app/                 # 依赖装配、路由和进程生命周期
│   ├── config/              # 环境变量配置和启动校验
│   ├── infra/               # MySQL、Redis、短信、微信、高德等基础设施实现
│   ├── modules/             # 按业务域拆分的 Handler、Service、Repository 和模型
│   └── pkg/                 # 日志、鉴权、幂等、指标等共享组件
├── migrations/              # Goose 数据库迁移，交付时需要完整保留
├── deploy/                  # 本地 Compose、数据库账号和部署辅助文件
└── docs/runbooks/           # 联调与运行文档
```

典型请求调用链为：

```text
HTTP 请求 → Router/Middleware → Handler → Service → Repository → MySQL/Redis/MQ
```

业务代码优先在 `internal/modules/<模块名>` 内闭环；外部服务适配放在 `internal/infra`，跨模块通用能力放在 `internal/pkg`。

## 配置说明

### 两个 MySQL DSN

- `JXE_MYSQL_MIGRATION_DSN`：高权限账号，仅供迁移和 Seed 使用，需要建表、改表等权限。
- `JXE_MYSQL_DSN`：低权限运行账号，供 API 和 Worker 日常读写业务数据。

两者分离可以避免业务进程意外修改数据库结构。Go 后端应使用独立数据库；不要让会自动同步表结构的旧服务与它共用同一个库。

### 本地 Mock

`.env.example` 默认开启微信认证、微信支付、支付和短信的本地 Mock，便于无第三方凭证时联调。高德地图相关接口需要有效的 Key；Key 为空时，依赖地图供应商的接口可能不可用。

售后模块默认关闭。需要联调售后页面时，可在 `.env.local` 中增加：

```dotenv
JXE_AFTERSALE_ENABLED=true
```

完整执行退款还需要开启 `JXE_REFUND_EXECUTION_ENABLED=true`，并确保对应支付/退款 Provider 已正确配置。修改环境变量后必须重启 API。

## 测试

```bash
make test
```

该命令执行项目的 Go 测试。依赖真实 MySQL、Redis 或 RabbitMQ 的集成测试可能还需要对应容器、迁移和测试开关。

## 生产环境注意事项

- 不要使用 `.env.example` 中的默认密码、JWT Secret 或 Mock Provider。
- 第三方密钥和生产密码应由部署环境的密钥管理系统注入，不要写入 Git。
- 每个实例必须使用稳定且唯一的 `JXE_INSTANCE_ID`，并分配 `0..1023` 范围内不重复的 `JXE_SNOWFLAKE_NODE_ID`。
- RabbitMQ/Outbox 采用至少一次投递，消费者必须继续按事件 ID 去重。

## 相关文档

- [API 手工验证与前端联调](docs/runbooks/api-manual-test.md)
- [OpenAPI 源文件](internal/modules/docs/openapi.yaml)
