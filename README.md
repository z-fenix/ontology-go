# ontology（S1 代码骨架）

本目录实现 Spec §3（元模型：加载、编译、校验）和 §4 中 `ontoctl validate` / `export` 两个命令，附带单元测试与示例 Ontology。

## 快速开始

```bash
go mod tidy                      # 拉取依赖并生成 go.sum
make fmt                         # gofmt（源码用空格缩进，需格式化一次）
make test                        # 运行全部测试
make validate-example            # 构建并校验 examples/ontology
./bin/ontoctl validate examples/ontology --format json
./bin/ontoctl export examples/ontology
```

## 目录

```
cmd/ontoctl/            CLI（cobra）：validate、export、version
internal/spec/
  model.go              编译后的元模型类型
  typeref.go            类型字符串解析：vector(n)、array<T>、decimal(p,s)
  loader.go             递归加载 YAML，保留行列号
  compile.go            编译和全部校验规则
  constraints.go        约束校验（VT002/VT003）
  diag.go               诊断结构、排序、计数
  util.go / yamlutil.go 命名规则、snake_case、拼写建议、YAML 节点工具
  *_test.go             表驱动测试（每条规则至少一个反例）
examples/ontology/      航空维修示例（4 个对象类型、3 个链接类型）
```

## 已实现的规则

G000–G007、VT001–VT003、OT001–OT013、OT015、LT001–LT005、AT001–AT011、WH001–WH002。

规则说明：
- OT011–OT015 和 LT005 是对 Spec 的补充：非法类型、非法 sensitivity、properties 为空、columnMapping 引用了未知属性、join table 引用了未声明的 connector。
- 字段名写错（G005）会直接报错，不会被静默忽略。
- `actionTypes`、`webhooks`、`migrations` 这三种顶层 kind 已完整编译：动作规则 AT001–AT010，webhook 规则 WH001–WH002。AT003（CEL 布尔表达式校验）依赖 `internal/expr` 通过 `spec.SetExprHooks` 注入的校验器。

## 与 Spec 的差异 / 留到 S2

- 结构校验靠代码里的字段白名单，没有用嵌入的 JSON Schema。
- 暂不包括：MG 系列规则、SARIF 输出。AT/WH 规则、CEL、diff、publish、ddl 均已实现。
- `Property.Column` 目前表示**源表**中的列名，物化表的 DDL 映射在 S2 由 `objstore` 生成。

## S1-B：objstore + 本地开发环境

```bash
make dev-up               # 启动 PostgreSQL、OpenFGA、Temporal（含 UI）、mock-mro，并执行数据库迁移
make ddl-example          # 打印对象表 DDL
make test                 # 单元测试（不依赖数据库）
make test-integration     # 集成测试（需先 dev-up；会删除并重建 obj schema）
make dev-down
```

| 服务 | 本地地址 |
|---|---|
| PostgreSQL | `localhost:5432`（onto/onto，库 `ontology`） |
| OpenFGA HTTP | `localhost:8090` |
| Temporal gRPC / UI | `localhost:7233` / `localhost:8233` |
| mock MRO（MockServer） | `localhost:1080` |

`internal/objstore` 的组成：
- `ddl.go`：`GenerateDDL` 和 `PlanDDL(old, new)` 生成可重复执行的建表、加列、外键索引和多对多链接表语句。列类型变化会直接报错，需要用 S2 的 castProperty 迁移处理。向量属性不写入 PostgreSQL。
- `codec.go`：按属性类型校验并转换值。decimal 和 date 用 text 传输；JSON 类型以 `::text::jsonb` 写入，读出后解析；timestamp 统一转成 UTC。
- `store.go`：
  - 读：`Get`、`GetMany`、`Find`（eq/in 条件，按主键分页）、`Links`（外键和多对多双向遍历）。
  - 写：`ApplyEdits(tx, edits)`。
    - CREATE 遇到主键冲突返回 `ErrAlreadyExists`。
    - MODIFY 和 DELETE 带乐观锁，版本不一致返回 `ErrStale`，对象不存在返回 `ErrNotFound`。
    - LINK 和 UNLINK：外键链接改写持有外键的那一侧对象；多对多链接 upsert 或软删除链接表中的行。
    - 同一批次多次修改同一对象时，版本号自动接续。
    - 出错返回 `*EditError`，带出错编辑的序号，事务由调用方回滚。
- 物化列名是属性 apiName 的 snake_case，`spec.Property.Column` 仍然表示**源表**列名。
- S1 限制：已软删除对象的主键不能再次 CREATE；查询条件只支持 eq/in。

## S2：Action 规则 + CEL 表达式 + diff

```bash
go mod tidy && make fmt
make test                 # 包含 spec 与 expr 的单元测试
./bin/ontoctl validate examples/ontology
./bin/ontoctl export examples/ontology > /tmp/v1.json
# 改动 examples/ontology 后，和上一版对比，破坏性变更缺迁移时退出码为 1：
./bin/ontoctl diff examples/ontology --against /tmp/v1.json --format markdown
```

新增：
- **`internal/spec/action.go`**：Action 类型、参数、规则、副作用、webhook、迁移的模型定义。`CompiledOntology` 现在带 `ActionTypes` / `Webhooks` / `Migrations`。
- **`internal/spec/compile_action.go`**：AT001–AT010、WH001–WH002 校验。参数支持 `objectRef<T>`、`objectRefList<T>` 和标量；规则支持 createObject / modifyObject / deleteObject / addLink / removeLink；副作用限制至多一个 WRITEBACK webhook。
- **`internal/expr`**：基于 cel-go 的表达式环境。`submissionCriteria` 必须编译为 bool，模板里的每个 `{{...}}` 都会校验。objectRef 参数作为动态 map，支持 `now()`、`uuid()` 和成员运算符 `in`。通过 `bridge.go` 的 init 注入到 spec，避免循环依赖——`spec` 不反向依赖 `expr`。
- **`internal/spec/diff.go`**：`Diff(old, new)` 列出增删改，并标记破坏性变更；`MigrationCoverage` 找出缺少迁移声明的破坏性变更（对应 PUB002）。收紧 enum、删属性、改主键、改类型都算破坏性；加可选属性、加类型、加 Action 不算。
- **`ontoctl diff --against <json> [--format text|json|markdown] [--allow-constraint-tightening]`**：存在未声明迁移的破坏性变更时退出码为 1，可直接用于 CI。

与 Spec 的差异：
- Action 参数必须写成 `{ type: ... }` 的映射，不支持 `name: string` 简写。
- CEL 的多跳链接访问目前只在运行时解析；编译期不校验链接路径（留到 Action Service 集成时做）。
- webhook 模板的引用校验只检查括号是否配对，因为 webhook 定义本身不绑定到某一个 Action。

## S2：OMS 发布服务（发布状态机 + 元数据 API）

```bash
go mod tidy && make fmt
make test                 # 含 omsclient 缓存单测
make dev-up               # PG + 迁移
make test-integration     # 含 oms 发布状态机集成测试
make run-oms              # 本地启动 OMS（读 ONTO_DB_DSN）
```

发布（ontoctl 把源文件 POST 给 OMS，服务端重新编译校验）：
```bash
curl -s -X POST localhost:8080/api/v1/ontologies/mro/versions \
  -H 'X-Actor: me@acme' \
  -d "{\"gitCommit\":\"$(git rev-parse HEAD)\",\"files\":[{\"path\":\"ontology.yaml\",\"content\":\"...\"}]}"
```

新增：
- **`internal/oms/store.go`**：版本、类型注册（RID）、按 checksum 查重、激活等持久化操作。发布时用 `SELECT ... FOR UPDATE` 锁住 ontology 行来串行化。
- **`internal/oms/service.go`**：发布状态机（Spec §5.3）——编译校验 → checksum 查重（幂等）→ 锁 + diff + 迁移检查（PUB002）→ 分配版本和 RID + 写 PENDING → 提交 → 在独立事务里应用 DDL → 激活并发 NOTIFY。破坏性变更缺迁移返回 409 PUB002；DDL 失败把版本标为 FAILED 并返回 PUB003。回滚（activate 旧版本）前会检查 DDL 是否兼容，不兼容则拒绝。服务端始终重新编译，不信任客户端产物。
- **`internal/oms/handler.go`**：REST API——发布、版本列表/详情、激活、compiled（带 ETag 和 If-None-Match）、对象类型、Action 类型（附参数 JSON Schema）。
- **`internal/oms/listener.go`**：LISTEN/NOTIFY 监听 `oms_version` 通道，断线重连。
- **`internal/omsclient`**：读侧客户端，带 LRU 版本缓存（默认 3 个版本）和 active 指针；收到版本变更通知后下次 `Active()` 会重新拉取。`HTTPLoader` 从运行中的 OMS 拉 compiled。Action Service 后续会用它。
- **`cmd/oms`**：HTTP 服务入口，优雅关停。
- **`internal/platform/apperr`、`httpx`**：统一错误类型（带 code 和 HTTP status）和 JSON 读写辅助。

RID 规则：同一 `(ontology, kind, apiName)` 跨版本复用同一个 RID；集成测试验证了加属性后 RID 不变。

与 Spec 的差异：
- `dropProperty` 迁移在 MVP 里只做软删除（保留物理列），DDL 本身只做加列和建表。
- 认证（JWT）尚未接入，发布者身份暂时从 `X-Actor` 头取;正式环境要换成 OIDC。

## S3：Action Service（执行引擎 + 提交 + 幂等 + 乐观锁）

```bash
go mod tidy && make fmt
make test                          # 含 internal/action 引擎单测（不依赖数据库）
make dev-up && make test-integration   # 含真实 PG 的提交、审计、并发幂等测试
make run-oms                       # 终端 1：:8080
make run-action                    # 终端 2：:8081（开发模式，授权放行）
curl -s -X POST localhost:8081/api/v1/ontologies/mro/actions/groundAircraft/apply \
  -H 'X-Actor: alice' -H 'Idempotency-Key: k1' \
  -d '{"parameters":{"aircraft":"B-1234","reason":"bird strike"}}'
```

执行流程（`internal/action/engine.go`，对应 Spec §7.3）：
1. 解析 ontology 版本和动作类型
2. 幂等检查：同一个 key、相同参数时直接返回原结果；参数不同返回 409 `IDEMPOTENCY_KEY_REUSED`
3. 参数解析：一次收集全部问题（未知参数、缺必填、类型或约束不符），填默认值
4. 加载引用对象：不存在和无权查看都返回 `OBJECT_NOT_FOUND`，避免泄露对象是否存在
5. 动作权限：Agent 本身和它代表的用户都要通过；授权服务不可用时返回 503，失败关闭
6. 逐条执行前置条件（CEL），返回每条的结果
7. 仅校验模式到这里结束，不写任何数据
8. 内置规则展开为编辑，或调用注册的 Go 函数（函数只能通过受权限约束的只读视图读数据，上限 10 万个对象）
9. 编辑后校验：属性存在、类型和约束、必填属性，单次最多编辑 1 万个对象
10. 一个事务内写入：`action_run` + 对象编辑（带乐观锁）+ `object_edit`（含编辑前快照）+ outbox 事件 + 待执行的副作用记录
11. 版本冲突时从第 4 步重读重试一次，仍冲突返回 409 `STALE_OBJECT`；并发请求用同一个 key 时，靠数据库唯一索引保证只提交一次，其余请求返回赢家的结果

接口（`handler.go`）：`/validate`、`/apply`、`/applyBatch`（每条独立事务，最多 100 条）、`GET /actionRuns/{rid}`（只返回给发起人，含副作用状态）。

新增文件：
- `internal/action/`：`engine.go`、`params.go`、`values.go`、`rules.go`、`function.go`、`pgcommit.go`、`handler.go`、`types.go`
- `internal/action/functions/ground_aircraft.go`：示例 Go 函数（停场 + 提升该机未关闭工单优先级 + 新建检查工单），示例 YAML 已改为 `runtime: go`
- `internal/authz/`：OpenFGA HTTP Check 客户端，以及仅供开发用的 `AllowAll`
- `cmd/action-service/`：服务入口；监听 `oms_version` 通知刷新本地缓存

**本迭代的限制（下一步 Temporal 迭代解决）：**
- 带 WRITEBACK webhook 的动作（例如示例里的 `assignWorkOrder`）目前直接返回 501 `WRITEBACK_NOT_SUPPORTED`，不会提交任何数据。
- 副作用（通知、SIDE_EFFECT webhook）只记录为 PENDING，还没有执行它们的 worker。
- 身份目前从 `X-Actor` 等请求头读取，仅限开发；正式环境必须换成 OIDC JWT 校验。
- TypeScript / Python 函数运行时未实现，只支持 `runtime: go`。
- 返回结果还没有做属性脱敏（Spec §8.2）。

## S3：Temporal 工作流 + Webhook 写回 + 补偿 + 副作用执行

```bash
go mod tidy && make fmt
make test                 # 含 webhook 执行器、工作流（Temporal 测试环境）和引擎单测
make dev-up               # 含 Temporal（:7233，UI :8233）和 mock-mro
migrate 000002            # dev-up 会自动执行；新增 action_run.plan 列
make run-oms && make run-action   # action-service --role=all：HTTP + Temporal worker
```

带写回的动作（例如 `assignWorkOrder`）现在的流程：
1. 引擎完成校验、生成编辑，并用当时的对象数据渲染好所有 webhook 和通知（URL、body、补偿调用）。
2. 把执行记录存为 `WRITEBACK_PENDING`，执行计划写进 `action_run.plan`。此时对象还没有修改。
3. 启动工作流 `action-run:<rid>`，最多同步等待 `ONTO_ACTION_SYNC_WAIT`（默认 10s）；超时返回 `WRITEBACK_PENDING`，之后可通过 `GET /actionRuns/{rid}` 查结果。
4. 工作流按顺序执行：
   - 先调外部系统写回。按 webhook 的 retry 配置重试（429、5xx、网络错误会重试，其他 4xx 不重试）。最终失败时执行记录为 `FAILED`，接口返回 424 `WRITEBACK_FAILED`，ontology 不变。
   - 写回成功后提交编辑。提交是幂等的，基础设施错误会重试；业务冲突（版本冲突、对象已存在等）不重试，直接进入补偿。
   - 提交失败时调用补偿接口，执行记录为 `COMPENSATED`，接口返回 409 和原始错误码。没有定义补偿接口或补偿失败时，标记为 `WRITEBACK_INCONSISTENT` 并打错误日志，需要人工处理。
   - 提交成功后启动副作用工作流 `side-effects:<rid>`。它独立于主工作流运行，单个副作用失败只记录，不回滚编辑。
5. 不带写回的动作仍然同步提交，提交后再启动副作用工作流。

每次调用外部系统都会写入 `side_effect_run`，包括请求、响应（截断到 8 KB）、尝试次数和状态。请求头带稳定的 `Idempotency-Key: <rid>:<name>:<mode>`，已成功的副作用重试时不会再发。

Webhook 执行器（`internal/webhook`）：
- 主机白名单 `ONTO_WEBHOOK_ALLOWED_HOSTS`（支持 `*.internal` 这种后缀通配），并且拒绝重定向，防止通过跳转绕过白名单（SSRF）。
- 认证方式：bearer、basic、oauth2_client_credentials（token 会缓存）。密钥目前只支持 `env://NAME`，`vault://` 会明确报错。
- 审计记录里只保存密钥引用，不保存密钥本身。

部署：`--role=api`（只跑 HTTP，负责启动工作流）、`--role=worker`（只跑 Temporal worker）、`--role=all`（本地开发用）。

已知限制：
- `vault://` 密钥未接入。
- 通知只写日志（`LogNotifier`），还没有真正的邮件或消息集成。
- 工作流已启动但服务在返回前崩溃时，执行记录会停在 `WRITEBACK_PENDING`，直到工作流完成后被更新；还没有清扫任务处理 Temporal 启动失败后遗留的 PENDING 副作用。
- 写回路径还没有基于真实 PG + Temporal 的端到端集成测试，目前靠单元测试和 Temporal 测试环境覆盖。

## ontoctl publish：打通发布流程

```bash
make run-oms                      # 终端 1：OMS :8080
make publish-example              # 终端 2：发布 examples/ontology
./bin/ontoctl publish examples/ontology --oms http://localhost:8080 --dry-run   # 只看变更，不建版本
```

流程：
1. 本地先编译校验（与 `ontoctl validate` 相同）。有错误直接退出，不发请求；`--skip-local-validation` 可跳过本地这一步，服务端仍会再校验。
2. 收集目录下所有 `*.yaml` / `*.yml`，路径一律为相对路径、`/` 分隔、排序后发送（总大小上限 8 MB）。
3. `POST {oms}/api/v1/ontologies/{apiName}/versions`，OMS 重新编译、diff、检查迁移、建表、激活。
4. 输出结果：
   - 新版本：`published mro version 3 (ACTIVE)`，并列出变更
   - 内容没变：`unchanged: mro version 2 already has this content`
   - `--dry-run`：只列出会发生的变更
   - 被拒绝（校验失败、PUB002 缺迁移、PUB003 建表失败）：打印错误码和明细，退出码 1

参数与环境变量：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--oms` | `ONTO_OMS_URL` | OMS 地址，必填 |
| `--git-commit` | `GITHUB_SHA` | 记录到版本上的提交号；`--dry-run` 时可省略 |
| — | `ONTO_TOKEN` | 作为 `Authorization: Bearer` 发送（为后续 OIDC 预留） |
| `--actor` | `ONTO_ACTOR` | 开发环境 OMS 用 `X-Actor` 识别发布者 |
| `--allow-constraint-tightening` | — | 不因收紧约束而拒绝 |
| `--format text\|json` | — | json 便于 CI 解析 |
| `--timeout` | — | 默认 2 分钟（发布要执行 DDL） |

CI 示例（GitHub Actions）：

```yaml
- run: ./bin/ontoctl validate ontology --strict
- run: ./bin/ontoctl publish ontology --oms $OMS_URL --dry-run      # PR 上只看变更
- if: github.ref == 'refs/heads/main'
  run: ./bin/ontoctl publish ontology --oms $OMS_URL                # 合并后真正发布；GITHUB_SHA 自动带上
  env: { ONTO_TOKEN: ${{ secrets.ONTO_TOKEN }} }
```

限制：OMS 目前只认 `X-Actor`，还不校验 `ONTO_TOKEN`；接入 OIDC 后才算有真正的发布权限控制。

## CI（GitHub Actions）

`.github/workflows/ci.yml` 在 push 到 main 和每个 PR 上运行两个 job：
- **unit**：gofmt 检查、`go build`、`go vet`（含 integration tag）、`go test -race`、校验示例模型并生成 DDL，覆盖率作为 artifact 上传。
- **integration**（unit 通过后）：起 PostgreSQL 16 服务容器，用 psql 按顺序执行 `migrations/*.up.sql`，再串行（`-p 1`）跑 objstore、oms、action 的集成测试——这几个包会重置同一个库的 schema，不能并行。

Temporal 工作流测试用 SDK 自带的测试环境，在 unit job 里跑，不需要 Temporal 服务。

首次提交前：本地没有 Go 时可以直接推，CI 会在缺少 `go.sum` 时自动 `go mod tidy`（并给出 warning）；但 gofmt 检查会失败，因为部分源码还没格式化。建议在有 Go 的机器上先执行 `go mod tidy && make fmt`，把 `go.sum` 一起提交。

## 认证（OIDC JWT）与属性脱敏

OMS 和 Action Service 现在都通过 `internal/auth` 认证调用方，只依赖 Go 标准库：

- 通过 `{issuer}/.well-known/openid-configuration` 找到 JWKS。签名密钥会缓存，遇到未知的 `kid` 时自动重新拉取，以支持密钥轮换。
- 只接受 RS256/384/512 和 ES256/384；`none` 和 HMAC 一律拒绝。校验 `iss`、`aud`、`exp`（必填）和 `nbf`，允许 60 秒时钟偏差。
- issuer 必须是 https，本机地址除外；开发环境可以设置 `ONTO_OIDC_ALLOW_HTTP=true` 放开。
- 身份映射：`sub` 是用户；带 RFC 8693 `act.sub` 的委托 token 视为 AI 代理，代表 `sub` 对应的用户执行；`actor_kind=SERVICE` 视为服务账号。用户组从 `groups` claim 读取，可配置。

| 环境变量 | 用于 | 说明 |
|---|---|---|
| `ONTO_OIDC_ISSUER` / `ONTO_OIDC_AUDIENCE` | 两个服务 | 生产环境必填；audience 可以用逗号分隔多个 |
| `ONTO_OIDC_GROUPS_CLAIM` / `ONTO_OIDC_KIND_CLAIM` | 两个服务 | 默认 `groups` / `actor_kind` |
| `ONTO_AUTH_DEV_HEADERS=true` | 两个服务 | 开发模式：信任 `X-Actor` 等请求头，`make run-oms` / `run-action` 已默认开启 |
| `ONTO_OMS_OWNER_GROUPS` | OMS | 能发布和激活版本的组；为空时任何已登录用户都能发布（启动时会警告） |
| `ONTO_OMS_TOKEN` / `ONTO_OMS_ACTOR` | Action Service | 读取 OMS 元数据时用的服务 token，或开发模式下的身份 |

OMS 的所有 `/api/v1` 接口都要求登录，未登录返回 401；发布和激活版本还要求属于 owner 组，否则返回 403。`ontoctl publish` 已经会把 `ONTO_TOKEN` 作为 Bearer token 发送。

**属性脱敏**（`internal/authz/redact.go`）：`ParseClearances("grp-finance=CONFIDENTIAL,...")` 把用户组映射到可读的最高敏感级别，所有已登录用户默认都能读 INTERNAL；`Redact` 把超过该级别的属性置为 null，并返回被隐藏的属性名列表（对外以 `__redacted` 字段返回）。Action Service 的返回结果目前只包含对象引用、不含属性值，所以还没有地方用到脱敏；后续的对象查询接口会接入。

**升级注意：** 在部署时设置 `ONTO_OIDC_*` 或 `ONTO_AUTH_DEV_HEADERS=true` 之前，两个服务都会拒绝启动。这是有意的：不允许在没有认证配置的情况下误上线。

## 数据同步管道（Indexer）

```bash
make dev-up                 # 迁移 000003 新建 indexer.sync_state
make run-oms                # 终端 1
make publish-example        # 发布示例模型（建出 obj.* 表）
make run-indexer            # 同步一轮，输出 JSON 统计
./bin/indexer               # 常驻：每 30 秒同步一次
```

把源系统表里的数据合并进对象表（`obj.*`），每种对象类型按配置的冲突策略合并：

| 策略 | 合并规则 |
|---|---|
| `APPLY_USER_EDITS`（默认） | 用户改过的属性一直保留用户的值，源数据只更新其余属性 |
| `MOST_RECENT_VALUE` | 源数据行比对象最后一次编辑新时，覆盖所有属性（包括用户改过的），这些属性不再算“已编辑”；否则同上 |

两种策略共同的规则：
- 用户通过 Action 删除的对象不会因为源数据还在而复活；源系统删除的对象软删除，源数据行重新出现时恢复。
- 只由 Action 创建、源系统里没有的对象，不会被同步删除。
- 值没有变化时不写库，版本号不变；值变了版本号 +1，正在执行的 Action 会因乐观锁冲突而重读。

同步方式：
- **增量**：对象类型配了 `timestampColumn` 时，按 (时间戳, 主键) 游标分页读取变化的行。游标和数据在同一个事务里提交，进程中途崩溃也不会丢数据或重复写。
- **全量快照**：没有时间戳列的类型每轮都做全量；有时间戳列的类型每 `ONTO_INDEXER_FULL_INTERVAL`（默认 1 小时）做一次，用来发现源系统里被硬删除的行。整次快照在一个事务里提交。
- **防误删保护**：全量快照要删除超过一半的对象（至少 10 个时才检查）会被拒绝，避免源查询出错时清空数据；确认源数据没问题后用 `--force` 执行。
- **多对多关系**：每轮全量同步关联表。用户通过 Action 建立或解除的关系不会被源数据覆盖。
- **Debezium**：`ParseDebezium` + `Indexer.ApplyEvent` 已经能处理 Debezium 的 JSON 变更事件（新增、更新、删除、快照读取），走同一套合并逻辑。还缺 Kafka 消费者——为了不改动 `go.sum` 没有引入 Kafka 客户端，需要时在有 Go 环境的机器上加依赖即可接入。

配置：`ONTO_DB_DSN`（对象库）、`ONTO_SOURCE_<CONNECTOR>_DSN`（每个 connector 一个源库，如 `ONTO_SOURCE_MRO_DB_DSN`）、`ONTO_OMS_URL` / `ONTO_OMS_TOKEN`、`ONTO_INDEXER_INTERVAL`（默认 30s）、`ONTO_INDEXER_FULL_INTERVAL`（默认 1h）。

限制：
- 源库目前只支持 PostgreSQL。
- `MOST_RECENT_VALUE` 按“对象最后一次编辑时间”比较，不是按单个属性的编辑时间。
- 派生属性（如向量 embedding）不从源读取，也还没有生成。
- 同步不检查必填属性：源数据缺必填值时照样写入。

## 对象查询服务（query-service）

```bash
make run-oms && make publish-example && make run-indexer   # 先有数据
make run-query                                              # :8082
curl -s -X POST localhost:8082/api/v1/ontologies/mro/objects/WorkOrder/search \
  -H 'X-Actor: alice' -d '{
    "where": {"and": [
      {"eq": {"field": "status", "value": "OPEN"}},
      {"link": {"path": "aircraft", "where": {"eq": {"field": "baseAirport", "value": "SIN"}}}}
    ]},
    "orderBy": [{"field": "priority", "direction": "desc"}],
    "pageSize": 20}'
```

| 接口 | 说明 |
|---|---|
| `GET /objects/{type}/{pk}` | 单个对象 |
| `GET /objects/{type}/{pk}/links/{link}` | 沿关系导航（外键双向、多对多） |
| `POST /objects/{type}/search` | 过滤 + 排序 + 分页 |
| `POST /objects/{type}/aggregate` | 分组聚合：`count`、`sum`、`avg`、`min`、`max` |

均位于 `/api/v1/ontologies/{ont}` 之下；`?ontologyVersion=` 可固定版本，响应头 `X-Ontology-Version` 返回实际使用的版本。

**过滤语法**：`and`、`or`、`not`、`eq`（值为 null 时等价于 isNull）、`gt`、`gte`、`lt`、`lte`、`in`、`contains`（字符串不区分大小写；数组判断是否包含某元素）、`startsWith`、`isNull`、`link`（存在至少一个关联对象满足子条件，最多嵌套 3 层）。每个条件只能有一个运算符；字段名和值的类型都会按模型校验，所有值都用参数绑定，不拼接 SQL。

**权限与脱敏：**
- 配了 `authz` 的对象类型逐个检查 `viewer` 关系；看不到的对象和不存在的对象返回同样的 404。AI 代理需要它自己和它代表的用户都有权限；授权服务不可用时返回 503。
- 超出调用方级别的属性返回 null，并列在 `__redacted` 里（`ONTO_CLEARANCE_GROUPS=组=级别,...`，所有登录用户默认能看 INTERNAL）。这样验收用例 E16 有了落脚点。
- 不允许用超出级别的属性做过滤、排序或聚合，返回 403 `FIELD_NOT_PERMITTED`，否则可以通过“哪些对象匹配”推断出被隐藏的值。

**分页**：分页令牌记录扫描位置，并绑定查询条件和 ontology 版本，换了条件或版本后旧令牌返回 400。被权限过滤掉的对象不会让后续页错位；一次请求最多扫描 1 万个候选对象，超过时返回已找到的结果和继续扫描的令牌。

**限制：**
- 权限过滤是“先查再逐个检查”，受保护的对象类型数据量很大、而调用方能看到的很少时会比较慢；下一步用 OpenFGA 的 ListObjects 或预先计算的可见范围下推到 SQL。
- 受保护的对象类型做聚合时，最多只统计 1 万个匹配对象，超出返回 422。
- 只按偏移量分页，翻页过程中数据有增删时可能重复或漏掉个别对象。
- 还没有全文检索和向量语义检索（要等 OpenSearch）。

## CEL 多跳关系访问（AT011）

Action 的前置条件、规则模板和 webhook 模板现在可以沿单值关系访问关联对象，例如：

```yaml
submissionCriteria:
  - expr: 'workOrder.aircraft.status == "ACTIVE"'
    message: 飞机已停场，不能指派
rules:
  - modifyObject: { object: "{{workOrder}}", set: { summary: "{{workOrder.aircraft.model}} 检查" } }
```

- **编译期**（`spec/paths.go`，新规则 AT011）：检查路径里的每一跳都是存在的关系，只能沿单值关系走（不能走 `aircraft.workOrders` 这种一对多），最多 3 跳；写错会给出拼写建议。
- **运行时**（`action/links.go`）：只加载表达式真正用到的关联对象，逐跳检查 `viewer` 权限。关联为空、对象不存在、或调用方看不到时，该字段为 null——对它做比较会求值失败，前置条件按“不通过”处理，不会泄露看不到的对象是否存在。

## OpenFGA 权限关系同步（fga-sync）

按对象类型 `authz.tuples` 模板（如 `base:{{baseAirport}}`）从对象数据推导关系元组，并保持 OpenFGA 与之一致：

- `authz.derived_tuple` 记录“由对象数据推导出的元组”，属性改变或对象删除时只删除这些元组，手工授予或其他系统维护的元组不受影响。
- **增量**：读取 Action Service 写入的 `ObjectEdited` outbox 事件，处理到哪里记录在 `authz.sync_cursor`；失败时游标不前进，下一轮重试。
- **全量**：定期（默认 10 分钟）或 `fga-sync --once` 全量对账；Indexer 写入的源数据变更没有 outbox 事件，靠全量对账覆盖。
- 先写 OpenFGA 再更新记录表，写入和删除都按幂等方式提交（`on_duplicate`/`on_missing: ignore`），中途崩溃下一轮会修复；单次请求按 OpenFGA 默认上限 100 条分批。

配置：`ONTO_DB_DSN`、`ONTO_FGA_URL`、`ONTO_FGA_STORE_ID`、`ONTO_FGA_MODEL_ID`、`ONTO_OMS_URL`/`ONTO_OMS_TOKEN`、`ONTO_FGA_SYNC_INTERVAL`（5s）、`ONTO_FGA_FULL_INTERVAL`（10m）。迁移 000004 新建 `authz` schema。

限制：
- Indexer 带来的变更要等全量对账（最长 10 分钟）才反映到权限上。
- `on_duplicate`/`on_missing` 需要 OpenFGA 1.8 及以上（docker-compose 里是 1.8.1）。
- OpenFGA 授权模型（`.fga` 文件）本身还没有放进仓库，需要先手动创建 store 和模型。

## OpenFGA 授权模型与初始化

模型文件放在 `deploy/openfga/`：

| 文件 | 用途 |
|---|---|
| `model.json` | 授权模型（OpenFGA API 接受的 JSON 格式），`ontoctl fga init` 上传的就是它 |
| `model.fga` | 同一模型的 DSL 写法，方便阅读和评审；修改时两份要同步（`fga model transform` 可以把 DSL 转成 JSON） |
| `dev-tuples.json` | 开发用的示例授权：alice 是 SIN 基地经理，bob 是 SIN 工程师，carol 是 PEK 经理 |

模型结构：`base`（manager ⊇ engineer）→ `aircraft`（经 `base` 关系继承：工程师可查看、经理可停场）→ `work_order`（指派的技师可查看；能看飞机的人能看工单；能停场的人能指派）。

**一致性检查** `ontoctl fga check [dir] --model deploy/openfga/model.json`（不连 OpenFGA，已加进 CI）：
- 每个对象类型的 `authz.fgaType` 在模型里存在，并且有 `viewer` 关系（查询服务和 Action 引擎都用它判断可见性）。
- 每条 `authz.tuples` 的关系存在、可以直接写入，并且接受模板里的用户类型（比如 `base:{{baseAirport}}` 要求该关系允许 `base` 类型）。
- 每个 Action 的 `permission.relation` 在对应类型上存在。

**初始化** `ontoctl fga init [dir] --fga-url ... [--store ontology] [--seed file]`：先做上面的检查；store 不存在就创建；当前最新模型和文件内容不同才上传新模型；可选写入示例授权。重复执行不会产生新 store 或新模型。标准输出是可直接 `eval` 的环境变量：

```bash
make dev-up            # 已包含 make fga-init，ID 写入 .env.fga
cat .env.fga           # export ONTO_FGA_STORE_ID=... ONTO_FGA_MODEL_ID=...
make publish-example && make run-indexer
make fga-sync-once     # 从对象数据推导 base/aircraft/assignee 关系
make run-action-fga    # Action Service 走真实 OpenFGA 授权
```

`ONTO_FGA_TOKEN` 设置后会作为 [REDACTED_TOKEN] 发给 OpenFGA（OpenFGA 开启 preshared key 认证时用）。

限制：
- `model.fga` 和 `model.json` 不会自动比对是否一致，CI 检查的只是 `model.json`。
- 模型变更只追加新版本，不删除旧版本；服务用的版本由 `ONTO_FGA_MODEL_ID` 决定，改了模型要更新这个变量再重启服务。

## MCP Server（AI 代理接入）

实现 MCP 协议 **2026-07-28** 版本：无状态 Streamable HTTP，端点 `/mcp` 只接受 POST，没有 initialize 握手和会话，每个请求在 `params._meta` 里带协议版本和客户端能力。

```bash
make run-oms; make run-query; make run-action   # 后端
make run-mcp                                      # http://127.0.0.1:8083/mcp
```

**工具**（`tools/list` 按固定顺序返回，根据当前 ontology 版本自动生成）：

| 工具 | 说明 |
|---|---|
| `ontology_describe` | 对象类型、属性（含敏感级别和枚举值）、关系、可用动作；建议代理先调用 |
| `objects_get` / `objects_links` | 读对象、沿关系导航 |
| `objects_search` / `objects_aggregate` | 筛选排序分页（单页最多 100 条）、分组统计 |
| `action_validate` | 只校验动作的参数、权限和前置条件，不写数据 |
| `action_<apiName>` | 每个 `ai.exposeToAgents: true` 的动作一个工具；参数 schema 由模型生成（类型、枚举、正则、范围），描述里带 `agentHint` 和前置条件说明 |

**权限**：MCP Server 自己不连数据库，所有调用都带着调用方的凭证（`Authorization`，开发模式下是 `X-Actor` 等）转发给 query-service 和 action-service，由它们做认证、对象权限和属性脱敏。所以代理能看到和能做的，和它代表的用户完全一致；AI 代理用 RFC 8693 委托 token 时，两边的权限都会检查。

**人工确认**（`ai.requireHumanConfirmation: true`），按协议的 Multi Round-Trip Request 实现：
1. 第一次调用先跑 `validate`；校验不通过直接返回错误，不会让用户确认一个注定失败的操作。
2. 校验通过返回 `resultType: input_required`，内含 `elicitation/create` 表单（动作名称、说明、全部参数），以及签名的 `requestState`。
3. 客户端把表单展示给用户，带着用户的回答和 `requestState` 重试。`requestState` 用 HMAC 签名，绑定调用者、工具和参数，10 分钟有效；参数被改动、换了调用者或过期都会被拒绝。
4. 用户确认后才执行，幂等键由这次确认派生，客户端重试同一次确认不会执行两次；用户拒绝则返回 `DECLINED`，不做任何修改。
5. 客户端没有声明 `elicitation` 能力时，直接返回 `MissingRequiredClientCapability`（-32021）。

**协议校验**：`MCP-Protocol-Version` 头必须存在并与 body 一致；`Mcp-Method`、`Mcp-Name` 头必须与 body 一致（支持 `=?base64?...?=` 编码），否则 400 + HeaderMismatch（-32020）；不支持的版本返回 UnsupportedProtocolVersion（-32022）并列出支持的版本；校验 `Origin` 头防 DNS rebinding（白名单 `ONTO_MCP_ALLOWED_ORIGINS`，不带 Origin 的非浏览器客户端放行）；GET/DELETE 返回 405；默认只监听 127.0.0.1。

配置：`ONTO_SERVER_ADDR`（默认 `127.0.0.1:8083`）、`ONTO_QUERY_URL`、`ONTO_ACTION_URL`、`ONTO_OMS_URL`/`ONTO_OMS_TOKEN`、`ONTO_MCP_ALLOWED_ORIGINS`、`ONTO_MCP_CONFIRM_SECRET`（十六进制，至少 16 字节；未配置时需要确认的动作一律拒绝）、以及与其他服务相同的 `ONTO_OIDC_*` / `ONTO_AUTH_DEV_HEADERS`。

限制：
- 只支持 2026-07-28 版本，不兼容 2025-11-25 及更早需要 initialize 握手的客户端（这类客户端会收到 400）。
- 不支持 `subscriptions/listen`，模型发布新版本后客户端靠 `ttlMs`（60 秒）过期重新拉取工具列表。
- 没有实现 MCP 的 OAuth 授权发现（Protected Resource Metadata）；客户端需要直接配置 bearer token。
- 写回类动作耗时超过 10 秒时，工具结果是 `WRITEBACK_PENDING`，代理需要之后再查询结果。

## TypeScript SDK 生成

```bash
./bin/ontoctl sdk generate examples/ontology --out web/src/mro.ts --ontology-version 3 --header "commit $(git rev-parse --short HEAD)"
make sdk-example      # 生成示例 SDK 并用 tsc --strict 做类型检查（需要 Node）
```

生成一个**单文件、零依赖**的 TypeScript 模块：运行时只用全局 `fetch`（Node 18+ 和浏览器都有），直接放进前端或 Node 项目即可。

```ts
import { createClient, OntologyError } from "./mro";

const client = createClient({ queryUrl: "https://onto.example.com", token: () => getAccessToken() });

const page = await client.objects.WorkOrder.search({
  where: { and: [
    { eq: { field: "status", value: "OPEN" } },                       // field 只能是 WorkOrder 的属性名
    { link: { path: "aircraft", where: { eq: { field: "baseAirport", value: "SIN" } } } },
  ] },
  orderBy: [{ field: "priority", direction: "desc" }],
});
page.data[0]?.properties.status;   // "OPEN" | "IN_PROGRESS" | "DONE" | "CANCELLED" | null

for await (const wo of client.objects.WorkOrder.all({ where: { isNull: { field: "technicianId" } } })) { /* 自动翻页 */ }

await client.actions.assignWorkOrder.apply({ workOrder: "WO-1", technician: "T-1" }, { idempotencyKey: "k1" });
```

生成内容：
- 每个对象类型：`XxxProperties`（属性类型）、`XxxLink`（可导航的关系名）、`Xxx`（完整对象类型）。枚举（含值类型里的枚举）生成字面量联合类型；精确小数、日期、时间戳是字符串；数组带元素类型；所有属性都可能为 `null`（无值或被脱敏），敏感属性在注释里标明。向量属性不生成（查询服务不返回）。
- 每个动作类型：`XxxParams`，必填参数非可选，对象引用是主键字符串；注释里带动作说明和前置条件。
- `createClient()` 返回 `client.objects.<类型>`（`get` / `find` / `search` / `all` / `aggregate` / `links`）和 `client.actions.<动作>`（`validate` / `apply` / `applyBatch`）。错误统一抛 `OntologyError`，带稳定的 `errorCode`（如 `SUBMISSION_CRITERIA_FAILED`、`STALE_OBJECT`）。
- 文件头记录 ontology 名、版本、checksum；输出是确定的，可以提交进仓库并在 CI 里比对是否过期。

**CI**：每次构建都会生成示例 SDK，并用 `tsc --strict` 编译 `examples/sdk/usage.ts`。这个示例同时断言错误用法**编译不过**：属性名写错、缺少必填参数、关系名写错（`@ts-expect-error`）。

限制：
- 只支持 TypeScript；Python/Java SDK 还没做。
- 筛选条件里 `link.where` 的字段名不做类型检查（关联类型在运行时才确定），由服务端校验。
- 生成的客户端按模型生成时的类型工作；模型发布新版本后要重新生成，服务端会拒绝不兼容的调用，但不会自动提示 SDK 过期。

## 写回路径端到端测试（E07 / E09 / E10 / E11）

`internal/e2e/writeback_test.go` 用真实组件跑完整的写回流程：真实的 Action 引擎和 PostgreSQL 提交器、真实的 Temporal 服务和 worker、真实的 webhook 执行器，外部维修系统用本地 HTTP 服务模拟。

| 用例 | 场景 | 断言 |
|---|---|---|
| E07 | 写回成功 | 工单变为 IN_PROGRESS 并指派 T-1；MRO 收到一次 `/assign`，body 和 `Idempotency-Key` 正确；通知副作用执行且 `side_effect_run` 为 SUCCEEDED |
| E11 | 同一幂等键提交两次 | 返回同一个 actionRid，MRO 只被调用一次 |
| E09 | MRO 返回 500 | 按 `retry.max` 重试 3 次后返回 `WRITEBACK_FAILED`；工单版本和属性都不变；执行记录 FAILED；不发通知 |
| E10 | 写回期间工单被并发修改 | 提交时版本冲突，调用 `/unassign` 补偿；返回 `STALE_OBJECT`；执行记录 COMPENSATED；工单只保留并发修改的那次变更；不发通知 |

Temporal 的来源：
- 设置了 `ONTO_TEST_TEMPORAL`（如 `localhost:7233`）就连这个服务；`make test-integration` 默认用 `make dev-up` 起的 Temporal。
- 没设置时由 Temporal SDK 的 `testsuite.StartDevServer` 自动启动开发服务器，首次会下载 Temporal CLI（CI 里缓存下载结果）；也可以用 `TEMPORAL_CLI_PATH` 指定已有的 CLI。

示例模型里写回 webhook 的地址是 `https://mro.internal`，测试会把它改成本地模拟服务的地址并关闭认证，不影响模型文件本身。

## Action Service 运维功能

### 卡住执行清扫
worker 进程里定期扫描两类卡住的执行，默认每分钟一次，只处理 5 分钟没有更新的记录：
- **写回执行**停在 `WRITEBACK_PENDING` 或 `COMPENSATING`，比如保留执行之后、启动工作流之前进程退出了。清扫器重新派发工作流：工作流 ID 由 actionRid 派生，已在运行或已结束的工作流只会被关联上，不会重复执行。工作流已结束但结果没记下来的，补记结果。
- **已提交执行的副作用**还是 `PENDING`，比如提交后副作用工作流没启动成功。清扫器重新启动副作用工作流，已成功的副作用不会重发。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ONTO_SWEEP_INTERVAL` | `1m` | 扫描间隔；设为 `off` 关闭 |
| `ONTO_SWEEP_OLDER_THAN` | `5m` | 记录多久没更新算卡住 |

多副本同时扫描是安全的，原因同上。

### 审计查询
| 接口 | 说明 |
|---|---|
| `GET /api/v1/ontologies/{ont}/actionRuns` | 按 `actor`、`actionType`、`status`、`objectType`+`objectPk`、`since`/`until`（RFC 3339）筛选，按创建时间倒序；`pageSize` 1–200（默认 50），用 `nextPageToken` 翻页 |
| `GET /api/v1/ontologies/{ont}/actionRuns/{rid}` | 单个执行及其副作用状态 |
| `GET /api/v1/ontologies/{ont}/actionRuns/{rid}/edits` | 该执行的对象编辑（变更和修改前的值） |

可见范围：`ONTO_AUDIT_GROUPS` 里的组成员（审计员）能看所有执行；其他人只能看自己发起的执行，以及 Agent 代表自己发起的执行。非审计员按别人的 `actor` 筛选返回 403，看别人的单个执行返回 404。

### 限流
按"调用者 × 动作类型"做令牌桶限流，作用于 validate、apply 和 applyBatch（批量请求按条数扣令牌）。超限返回 `429 RATE_LIMITED`，带 `Retry-After` 头。

| 环境变量 | 格式 | 说明 |
|---|---|---|
| `ONTO_RATE_LIMIT` | `<每分钟>[/<突发>]`，如 `60/10` | 默认限额；不设表示不限 |
| `ONTO_RATE_LIMIT_AGENT` | 同上 | AGENT 调用者的限额 |
| `ONTO_RATE_LIMIT_ACTIONS` | `assignWorkOrder=30/5,groundAircraft=2` | 按动作类型覆盖，优先级最高 |

限流状态在各副本内存里，N 个副本时实际上限最多是 N 倍。需要全局精确限流时，可以换成基于 Redis 等共享存储的实现。

### Vault 密钥
webhook 的 `secretRef` 现在支持 `vault://<路径>[#<字段>]`，从 Vault KV v2 读取：
- `vault://mro/client#token` 取字段 `token`。
- `vault://mro/client` 在只有一个 `value` 字段时取该值，否则返回整个密钥的 JSON（OAuth2 客户端凭证认证正好需要这种格式）。

| 环境变量 | 说明 |
|---|---|
| `VAULT_ADDR` | 设了才启用 Vault |
| `VAULT_TOKEN` | 静态令牌 |
| `VAULT_ROLE_ID` / `VAULT_SECRET_ID` | AppRole 登录，优先于静态令牌；令牌在租期 90% 时续期，被吊销时自动重登一次 |
| `VAULT_KV_MOUNT` | KV v2 挂载点，默认 `secret` |
| `VAULT_NAMESPACE` | Vault Enterprise 命名空间 |

读到的密钥缓存 5 分钟。`env://` 照常可用。

### TypeScript / Python 函数
函数动作现在可以用 `runtime: typescript` 或 `runtime: python`，跑在独立的运行时服务里，配置和调用协议见 [runtimes/README.md](runtimes/README.md)。两个运行时都没有第三方依赖，`make test-runtimes` 运行它们的测试。
