# GigMatch · 自由职业者撮合平台

连接需求方与自由职业者的撮合平台：需求发布、报价竞标、合同签订与项目交付全流程管理。

## 快速启动（Docker Compose 一键部署）

```bash
cp .env.example .env
docker compose up -d --build
```

- 前端：http://localhost:28030
- 后端 API：http://localhost:29068
- API 文档：http://localhost:29068/docs
- 健康检查：http://localhost:29068/healthz
- 就绪检查：http://localhost:29068/readyz

测试账号（密码均为 demo123456）：
- `requester01`（需求方）
- `freelancer01` / `freelancer02`（自由职业者）
- `admin`（管理员）

## 项目主要功能

- 需求大厅：瀑布流列表 + 预算/技能/状态多维筛选
- 需求详情：完整信息 + 报价列表（需求方视角）+ 报价提交表单（自由职业者视角）
- 我的工作台：分角色展示已发布需求、已报价项目、进行中合同
- 合同详情：条款、阶段进度（分阶段付款进度条）、双方信息
- 合同变更单：进行中/待确认完成的合同，任一方可发起变更（原因、范围、金额增减、阶段金额调整）；同一合同仅允许一张待处理变更；**已完成阶段的名称、金额、状态冻结不可改，只允许调整未完成阶段，且新总额恒等于已完成金额与未完成阶段金额之和**；另一方同意后在单事务内原子更新总额与阶段并回读校验（行锁 + 乐观锁版本号），可拒绝/撤回；待处理变更期间暂停完成合同；完成与审批并发时仅一方成功、失败方返回明确冲突，绝无部分更新或旧快照覆盖
- 个人资料：展示/编辑个人信息、技能标签、历史项目
- 横切：JWT 认证授权、操作日志、路由守卫、请求拦截器自动带 token

## 本地开发

### 后端（Go 1.22 + Gin + GORM）

```bash
cd backend
go mod tidy
go run ./cmd/server
```

构建检查：`go build ./...`；测试：`go test ./...`

合同变更并发集成测试（真实 MySQL 8 / InnoDB，**独立 TCP 连接池**、行锁与唯一索引；不使用内存库、mock 或单连接串行化）：

```bash
cd backend

# 1) 一键准备真实 MySQL（无需 root/Docker）。脚本会：
#    从 dev.mysql.com/get（302 到版本化 CDN）多源回退下载官方通用包，
#    断点续传（curl -C -）、SHA-256 校验、解压，自动提取 libaio，
#    初始化独立数据目录并启动监听 127.0.0.1:38109 的隔离实例。
./scripts/setup-integration-mysql.sh up

# 2) 运行集成测试（审批×完成、审批×拒绝、撤回×审批、双重审批+失败重试、守恒拒绝）
TEST_MYSQL_DSN="it:it_pwd@tcp(127.0.0.1:38109)/gigmatch_it?charset=utf8mb4&parseTime=true&loc=Local" \
  go test -tags integration -count=1 ./internal/service/
```

辅助命令与故障恢复：

```bash
./scripts/setup-integration-mysql.sh status   # 探测实例并打印 DSN
./scripts/setup-integration-mysql.sh dsn       # 仅打印 DSN
./scripts/setup-integration-mysql.sh down      # 停止（数据保留，再 up 无需重新下载/初始化）
./scripts/setup-integration-mysql.sh clean     # 停止并删除数据目录
```

- 下载/校验/解压/初始化/启动/TCP 连通任一阶段失败都会打印 `阶段[xxx]失败` 与具体恢复建议；已下载分片保留，直接重跑 `up` 即断点续传。
- 可用环境变量覆盖：`IT_HOME`、`IT_MYSQL_PORT`、`IT_MYSQL_DB/USER/PASS`、`IT_MYSQL_VERSION`、`IT_MYSQL_SHA256`。默认 DSN 与脚本一致时第 2 步可省略 `TEST_MYSQL_DSN`。

### 前端（Vue 3 + TypeScript + Element Plus + Vite）

```bash
cd frontend
npm install
npm run dev
```

## 技术栈

| 层级 | 技术 |
| --- | --- |
| 后端 | Go 1.22 + Gin + GORM |
| 数据库 | MySQL 8.0 |
| 认证 | JWT（golang-jwt/v5）+ bcrypt |
| 前端 | Vue 3 + TypeScript + Element Plus + Vite + Pinia |
| 部署 | Docker Compose（Nginx 反代 + 多阶段构建） |

## 目录结构

```
├── backend/
│   ├── cmd/server/main.go
│   ├── database/init.sql       # 建库脚本（建表由 GORM AutoMigrate）
│   └── internal/
│       ├── config/  ├── model/  ├── repository/  ├── service/
│       ├── handler/ ├── router/ ├── middleware/  ├── dto/
│       ├── constants/ ├── util/ ├── logger/      └── docs/
├── frontend/                   # Vue 3 前端（按提示词生成）
│   └── src/
│       ├── api/      ├── stores/      ├── types/
│       ├── components/common/ ├── hooks/
│       ├── pages/    ├── router/      ├── utils/
│       └── constants/
├── docker-compose.yml
└── .env.example
```

## 环境变量

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| COMPOSE_PROJECT_NAME | Compose 项目名 | gigmatch |
| FRONTEND_PORT / BACKEND_PORT / DB_PORT | 端口 | 28030 / 29068 / 33301 |
| DB_NAME / DB_USER / DB_PASSWORD / DB_ROOT_PASSWORD | 数据库配置 | gigmatch / gigmatch / gigmatch123 / root_pwd |
| JWT_SECRET | JWT 签名密钥，生产必须替换为 32 位以上随机值 | 见 .env.example |
| CORS_ALLOWED_ORIGINS | 允许跨域来源，逗号分隔；生产禁止 `*` | http://localhost:28030 |
| AUTH_RATE_LIMIT / API_RATE_LIMIT | 登录注册/业务接口限流（次/分钟/IP） | 10 / 120 |
| DB_MAX_OPEN_CONNS / DB_MAX_IDLE_CONNS | 数据库连接池 | 25 / 5 |
| DB_CONN_MAX_LIFETIME_MIN | 连接最大存活时间（分钟） | 5 |
| DB_CONNECT_RETRIES / DB_CONNECT_RETRY_INTERVAL_SEC | 启动重试次数/间隔 | 10 / 3 |

## Docker 部署说明

- 端口映射：前端 `${FRONTEND_PORT}:80`、后端 `${BACKEND_PORT}:8080`、MySQL `${DB_PORT}:3306`
- 前端 Nginx 将 `/api/` 反代到 `http://backend:8080/api/`
- 数据库命名卷 `db_data` 持久化；db → backend → frontend 健康依赖链
- 支持中文目录名部署（顶层 `name: gigmatch`）

## 枚举出现位置清单

| 枚举 | 后端定义 | 前端定义 | 前端消费 |
| --- | --- | --- | --- |
| RequirementStatus（draft/open/bidding/in_progress/pending_review/completed/cancelled） | `backend/internal/constants/requirement_status.go` | `frontend/src/types/enums.ts` | Requirements、RequirementDetail、Dashboard |
| BidStatus（pending/accepted/rejected/withdrawn） | `backend/internal/constants/bid_status.go` | `frontend/src/types/enums.ts` | RequirementDetail、Dashboard |
| ContractStatus（pending_signature/in_progress/pending_review/completed/terminated） | `backend/internal/constants/contract_status.go` | `frontend/src/types/enums.ts` | ContractDetail、Dashboard |
| ContractChangeStatus（pending/approved/rejected/withdrawn） | `backend/internal/constants/contract_change_status.go` | `frontend/src/types/enums.ts` | ContractDetail、ContractChangePanel、ContractCard、Dashboard |
| ContractChangeParty（party_a/party_b） | `backend/internal/constants/contract_change_status.go` | `frontend/src/types/enums.ts`（ContractChangeParty） | ContractChangePanel |
| UserRole（requester/freelancer/both/admin） | `backend/internal/constants/roles.go` | `frontend/src/types/enums.ts` | Layout、RequirementDetail、Dashboard |

## 主要 API 列表

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | /api/v1/auth/register | 注册 |
| POST | /api/v1/auth/login | 登录 |
| GET | /api/v1/auth/me | 当前用户 |
| GET/POST | /api/v1/requirements | 需求大厅/发布 |
| GET/PUT | /api/v1/requirements/:id | 详情/编辑 |
| POST | /api/v1/requirements/:id/status | 状态流转 |
| POST | /api/v1/requirements/:id/accept-bid | 采纳报价并生成合同 |
| GET/POST | /api/v1/bids | 报价列表/提交 |
| POST | /api/v1/bids/:id/withdraw | 撤回报价 |
| GET | /api/v1/contracts | 我的合同 |
| GET | /api/v1/contracts/:id | 合同详情 |
| POST | /api/v1/contracts/:id/sign · /complete | 签署/完成 |
| GET/POST | /api/v1/contracts/:id/changes | 变更历史/发起变更单 |
| GET | /api/v1/contracts/:id/changes/:changeId | 变更单详情 |
| POST | /api/v1/contracts/:id/changes/:changeId/approve · /reject · /withdraw | 同意（同步总额与阶段）/拒绝/撤回 |
| GET | /api/v1/dashboard | 我的工作台 |
| GET/PATCH | /api/v1/users/:id | 个人资料 |
| GET | /api/v1/operation-logs | 操作日志 |
| GET | /healthz、/readyz | 健康检查 |

统一响应格式：`{ "code": 0, "message": "ok", "data": ... }`

## License

MIT
